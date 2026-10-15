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
  by their role** — its card, re-authorizing it, who holds what; only owners remove one — and
  **read none of them by being one**. `read` passes only from an owner or an admin who reads the mailbox now;
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
  (`MAIL_KEYS_MAY_SEND=false`). It stops when its creator leaves the workspace, is disabled,
  suspended or deleted on the instance, or recovers their account (with their recovery code or a
  reset invitation). See [API keys](#api-keys).
- **Removing a mailbox keeps it 30 days**, used by nothing and found by nobody, recoverable by its
  workspace's owners, and then deletes it for good; everyone who read it, but whoever removed it,
  is told with a notice. A link that never connected is deleted at once. See
  [Removed mailboxes](#removed-mailboxes).
- A **closed team** is used by nobody until it reopens: its mailboxes reach nobody, its keys
  included, and sync nothing, keeping their index, consent and grants, and nothing in it changes.
  The operator closes a local team; a team from another source closes where it is managed. See
  [Closed teams](#closed-teams).
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
workspaces(id, kind, source, name, person_id, created_at, updated_at, closed_at)
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
- `kind`, `source` and `person_id` never change once the row is written: a trigger refuses any
  update that changes one of them (`workspaces_fixed`, migration 0016). Only a team's `name`, its
  `closed_at` and `updated_at` ever change.
- `closed_at` is when a team closed, 0 while it is open (migration 0019); only a team closes (a
  CHECK). See [Closed teams](#closed-teams).

A local team is deleted only with its last member (`user delete`), with its mailboxes; a team whose
source is not local outlives every person here, its last one included (see
[The workspace source](#the-workspace-source)). Moving a mailbox from one workspace to another is
not part of this phase.

### Members

```sql
workspace_members(workspace_id, user_id, role, status, created_at, updated_at)  -- PK (workspace_id, user_id)
```

- `role` is `owner`, `admin` or `member`; `status` is `active` or `disabled`.
- An **active member** is a membership with `status = 'active'` whose person is active on the
  instance (`users.status = 'active'`: neither disabled here nor suspended, see
  [People on the instance](#people-on-the-instance)). Every rule below that says "member" means an
  active one.
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
(`400`). A team's mailbox is its owners' to remove, a personal one its person's, an operator one the
operator's; an admin abandons only their own link that never connected. A removed mailbox is kept 30
days, recoverable, and then deleted for good ([Removed mailboxes](#removed-mailboxes)).

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
| `manage` | see the mailbox's card and re-authorize it | held by owners and admins through their role, on every mailbox of their workspace, who also change who holds what on it (owners also remove it); stored only for members |

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

A person who is not active is either **disabled here** or **suspended** (migration 0018).
`users.status` is `active` or `disabled`, and a disabled person whose `users.suspended_at` is set
is suspended: by an extension of the daemon, following a decision taken elsewhere
(`Service.SuspendPerson`; no route or command of the core suspends anyone). The schema holds a
suspended person disabled (a CHECK), so every rule that asks whether a person is active refuses
both alike: neither signs in, holds a session or a key, counts as a member, reads, waits for a key,
gives a consent or counts in a protection. What tells them apart is what they keep:

- **Disabled here** (`mailserver user disable`, or an owner of the instance signed in): every
  session ends, every key they created stops (`creator_disabled`), the invites they made that are
  still waiting expire, and every grant sealed to them goes (`docs/key-scheme.md` section 12.13);
  a migrated team consent still bound to them stops, with its index deleted. Their flags stay:
  switched back on, they wait for the key on every mailbox that has one.
- **Suspended:** the same ends — sessions, the tickets of their ceremonies, their reset invitations,
  their waiting invites, the consent attempts they started, and their keys, which say
  `creator_suspended` — and nothing they hold goes: memberships, flags and sealed grants stay,
  unusable while they are suspended. They read nothing and wait for nothing; nobody supplies them
  the key, gives them `read` with a grant or seals a first key to them; and they hold up no
  protection, nor does any protection hold up their suspension: no last owner, last reader or
  bound consent refuses it, and while they are suspended they are nobody's other owner or other
  reader. Their personal mailboxes, and a migrated team consent still bound to them, sync nothing
  meanwhile and keep their index; a team mailbox only they would read syncs nothing, and is not
  marked `no_reader`: it waits for them. **Resuming** them (`Service.ResumePerson`) makes them
  active again: they read every mailbox they hold `read` on and, on one that has a key, a grant at
  its current epoch (a first key written meanwhile sealed nothing to them, so they wait for it), and
  their mailboxes catch up. No session and no key comes back.
- **A disable here stands over a suspension.** Disabling a suspended person here runs the whole
  disable (their grants go, a bound consent stops, and the protections are asked again, but for the
  instance's last owner, which they no longer are, with the team mailboxes they would be the only
  reader of once resumed counted as theirs) and clears `suspended_at`, so a resume lifts nothing;
  suspending someone disabled here marks nothing; and switching a suspended person back on here is
  refused (`auth.ErrUserSuspended`): only a resume lifts a suspension. Once disabled here, the
  suspension is no longer recorded: switching the person back on here (`auth.Users.SetDisabled`,
  which no route or command calls) makes them active, and an extension that still holds them
  suspended suspends them again.

### Closed teams

A team may be **closed** (migration 0019, `workspaces.closed_at`): nothing in it is used until it
reopens, and nothing it holds is lost meanwhile.

- **Its mailboxes reach nobody**, its owners, its admins and its keys included: they are not
  listed, and every route about one answers `404`, as for a mailbox the caller holds nothing on.
  They **sync nothing** — the engine is told at once, and the eligibility rule refuses any batch
  still on its way — and **keep** their index, their consent to sync, every grant and sealed grant,
  and what the team's keys hold on them. Closing is a gate on using a mailbox
  (`store.OpenWorkspaceSQL`, beside the reading rule wherever a mailbox is reached and wherever it
  syncs), never part of who reads it: the reader rule, the last-reader protection and `no_reader`
  are what they were.
- **Its keys stay**, live and holding what they held, and reach nothing: a closed team's key still
  authenticates and lists its workspace, and nothing else. None is revoked for the closure.
- **Nothing in it changes**, by anyone, the operator included: nothing is linked, granted, keyed,
  invited or consented, and no member, invite, name, key or grant changes through the team, until
  it reopens (`409`). A key may still stop from outside it — its creator revoking it among their
  own keys, the operator revoking any key, its creator closed on the instance — since stopping one
  takes something away. The schema holds the part that gives something (triggers on `accounts`,
  `mailbox_access`, `mailbox_keys`, `mailbox_grants`, `api_keys`, `key_access` and `invites`);
  what takes something away, and every change to the memberships, stays allowed there, so that
  whatever keeps a team from another source in step with it applies them while it is closed.
- **Its members still list it**, with when it closed (`closed_at` in `GET /v1/workspaces`), and are
  told it is closed (`409`) for its members, invites, keys and who holds what; someone outside it
  is still told it does not exist (`404`). The operator still reads its members and who holds
  what.
- **Closing it ends** the invites into it still waiting (they expire) and the consent attempts on
  its mailboxes (their rows go; one already past its row stores nothing, since nobody manages a
  closed team's mailbox). Neither comes back when it reopens.
- **Reopened, it is as it was**: its mailboxes reach whoever holds them, catch up, and its keys
  reach what they hold.

**Who closes a team.** A local team is closed and reopened by the operator alone, with an
unrestricted instance admin key (`mailserver workspace close|reopen`,
`POST /v1/workspaces/{id}/close|reopen`): no person closes one, its owner included, and the console
has no such action. A team whose source is not local closes and reopens where it is managed; an
extension of the daemon follows it (`Service.CloseTeam`, `Service.ReopenTeam`, and
`workspace.SetClosedTx` for whatever applies the rest of such a change in one transaction), and the
operator is refused it (`409`, managed elsewhere). A personal workspace and the operator's never
close (`400`). The daemon lists the closed teams at start.

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

### Removed mailboxes

Removing a mailbox (migration 0020) keeps it **30 days**, recoverable, and then deletes it for good:
the hourly sweep does, never early, and at the daemon's start too, so that a database restored from
a backup deletes at once what is overdue (`Service.SweepRemovedMailboxes`).

- **Kept, and used by nothing.** A removed mailbox stays with its credentials, folders, messages,
  drafts, sends, key, grants and consent, and reaches nobody, its owners and the operator included:
  it is not listed, nor are a key's sends from it in that key's log, and every route about it
  answers `404`, but for what its owners do to a removed mailbox below. It is never dialled nor
  refreshed (`Registry.Mailbox` builds a mailbox only through `Repository.Get`, which leaves it out,
  and builds again, never caching, one it was building while the removal let go of it), syncs
  nothing, and has no reader (`store.LiveSQL` in the one rule), so it protects none: its readers may
  leave the team, be disabled or be deleted meanwhile. Nothing is granted, keyed or given on it (the
  schema's triggers). Its address stays taken in its workspace: linking it again is `409`, which
  says until when it is kept; an owner recovers it, or deletes it for good to link it again.
- **What goes at once**, and never comes back: what keys held on it — an instance key's
  restriction to it (one restricted to it alone is revoked, `restriction_emptied`, since a key
  restricted to nothing would reach every operator mailbox) and what a workspace key held — and the
  consent attempts under way on it, whose redirects then find nothing.
- **A link that never connected** — nothing was ever stored to authenticate with — is deleted for
  good at once instead, with nothing kept and, in a local workspace, nothing journaled; besides its
  owners, the person who linked it abandons it, an admin included. In a workspace from another
  source its deletion is journaled (`purged`) and its earlier entries are kept, their names
  forgotten, as a removed mailbox's are, since an extension reports that workspace's journal
  elsewhere ([Following an external directory](#following-an-external-directory)).
- **Who removes:** a team mailbox's active owners, an admin no longer; a personal mailbox's person;
  for an operator mailbox, an instance key with the `admin` scope.
- **Its readers are told.** Everyone who read it then, but whoever removed it, gets a notice in the
  console (`notices`), which names the mailbox by its address while it is kept and by a masked hint
  of it (`a…@example.com`) once it is deleted for good, says until when its owners may recover it,
  and stays until they dismiss it, or until its entry in the journal goes, at 90 days at most
  (below). A team mailbox's sync turned off tells its readers the same way — by an owner or an
  admin, or by the person a migrated consent was still bound to ([Consents](#consents)) — but
  whoever turned it off. A recovery withdraws the notices of the removal it undoes. A person's event
  stream says when they are given one (`Stream.NewNotices`), and a key has none.
- **Recovering it**, within its 30 days, is for its workspace's owners — a team's active owners, a
  personal mailbox's person, the operator for an operator mailbox (`RecoverAccount`). It comes back
  with its credentials, index, key, consent, and the readers who still qualify — an active member
  holding `read` and, on a mailbox that has a key, a grant at its current epoch — and without what
  keys held. Its sync is **held** (`accounts.sync_held_at`, `sync.held` on its card) until someone
  who may turns it on: its person for a personal mailbox (`PUT /v1/accounts/{id}/sync`
  `{"enabled": true}`, which takes no version and only lifts the hold), an owner or an admin for a
  team's, to the current sync text, the operator for an operator mailbox. It then catches up from
  the index it kept. A team mailbox nobody who could read it is left to read is not recovered
  (`409`): it is deleted for good and linked again, which gives the linker `read`. One that only a
  person suspended by an extension would read, once resumed, waits for them as a live one does
  (`no_reader` false in the list): it is recovered, its sync held, and turned on only once someone
  reads it. Nothing is recovered in a closed team (`409`), nor once its 30 days are over (`409`).
- **Deleting it for good** before its 30 days are over is for the same owners, repeating its id
  (`confirm`), and for the operator, any workspace's removed mailbox, to erase one when its person
  asks them to: the operator lists every workspace's removed mailboxes and never recovers a
  person's or a team's. A live mailbox is removed first (`409`). A closed team's removed mailboxes
  are deleted on time all the same, and its owners delete none meanwhile.
- **Its sync, removed:** a removed team mailbox's owners turn its sync off, which deletes the index
  it kept; nobody turns it on. Withdrawing a person's own sync consent deletes the index of their
  removed mailboxes too.
- **Closing a person** deletes at once, for good, the removed mailboxes that go with them — of
  their personal workspace and of the local teams they were alone in — with their journal.
- **Its grant at the provider, revoked.** A Google mailbox deleted for good — by the sweep, by its
  owners or the operator before, or with the person it goes with, deleted here or forgotten by a
  directory — has its grant revoked at Google's revocation endpoint (RFC 7009, the refresh token;
  `account.GoogleRevokeEndpoint`, which the daemon sets, and `mailserver user delete --bootstrap`,
  which deletes a person through the same method; nothing else does). The token is opened
  inside the transaction that deletes it and handed on in memory only; the revocation is sent once
  that commits, best effort, bounded (10 s), through a connection held to
  `MAIL_ACCOUNT_ALLOW_PRIVATE` as a mailbox's are, and logged without the token. A failure never
  stops the deletion, and is not retried: the grant then lapses when its person removes it. A grant
  another mailbox here still holds — the same address linked elsewhere through the same OAuth
  client, which Google keeps one grant for — is not revoked, which would cut that mailbox off.
  Microsoft's identity platform revokes no single grant, only every session of the person, and a
  password has nothing to revoke: their stored credential is deleted, and lapses.
- **The journal** (`mailbox_events`, with who read the mailbox then, `mailbox_event_readers`):
  removals, with the end they recorded (`purge_after`), syncs turned off, recoveries, deletions for
  good and strandings (migration 0021), each written in the transaction that makes it, never with
  the address, and kept 90 days. A mailbox is named by its display name, unless that holds an
  address or the local part of its own (three characters or more, without regard to case:
  "Support" for `support@…` is no name), and by a masked hint. A **stranding** is a team mailbox
  left with nobody who reads it, would read it once resumed, or would read it but for a hold,
  because the one person who did lost it in a way no protection refuses: a membership or a person
  an external directory removed, or a person disabled, deleted or reset here with force. It says
  why (`cause`: `membership_ended`, `forgotten`, `disabled`, `deleted`, `reset`) and whom it lost
  (`lost_user`), and tells nobody in the console. A sync turned off because a consent bound to a
  person stopped with them says so too (`cause` `bound_stopped`): a pause the server makes on its
  own. A deletion for good forgets the mailbox's name there, and its masked hint once no notice
  needs it; closing a person, and abandoning a link that never connected in a local workspace,
  delete its entries; deleting a person forgets that they acted, that a stranding lost them, and
  that they removed a mailbox still kept — at once, unless an external directory follows them, in
  which case the journal keeps their name until an extension releases them, 90 days at most.

| | Remove | See the removed ones, recover | Delete for good | Turn a recovered one's sync on |
|---|---|---|---|---|
| Personal mailbox | its person | its person | its person; the operator | its person |
| Team mailbox | an active owner | its active owners | its active owners; the operator | its owners and admins, to the current sync text |
| Operator mailbox | an instance key, `admin` scope | an unrestricted instance admin key | the same | the same |
| A link that never connected | its owners, or whoever linked it: deleted at once | — | — | — |

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
  mailbox's index for every reader — its messages, folders and events — and tells each of them but
  whoever turned it off, with a notice ([Removed mailboxes](#removed-mailboxes)); the service
  compacts and scrubs. Either way, on or off, it lifts the hold a recovery put on its sync.
- A person's withdrawal of their own sync consent deletes the index of **their personal
  mailboxes only**; it never reaches a team's (but for a migrated consent still bound to them,
  below).
- Disabling, removing or closing a person does not stop a team mailbox of a team others are in,
  nor remove it: it is the team's — but for a migrated consent still bound to them, below, and a
  mailbox only they read, which no reader is left for. Disabling a person stops their personal
  mailboxes (the eligibility rule requires the person active); deleting them removes those, and
  every local team whose only member they were, with its mailboxes. The open sync text says both.
- **Suspending a person** stops nothing and deletes nothing: their personal mailboxes, and a
  migrated consent still bound to them (below), sync nothing while they are suspended, since the
  eligibility rule requires the person, and the person a bound consent is bound to, active; each
  keeps its index and its consent, and catches up once they are resumed. A team mailbox only they
  would read waits for them the same way.
- A **closed team**'s mailboxes sync nothing, whatever their consent and readers, and nobody turns
  their sync on or off meanwhile: each keeps its index and its consent, and catches up once the
  team reopens ([Closed teams](#closed-teams)).
- A team mailbox **nobody can read** syncs nothing more; its consent stays, and its owners and
  admins see it marked (`no_reader`). One a suspended member would read once resumed is not marked:
  it waits for them. Read passes only from someone who reads it, so its sync is
  never turned on again (`409`), and the way out is an owner removing it and deleting it for good
  (it is not recovered while nobody would read it), and linking it again (the linker gets `read`);
  turning it off deletes what is still indexed.
- **A migrated consent stays bound to its linker.** Migration 0011 copied each team mailbox's
  linker's own consent (when the linker was active and had agreed) to the mailbox, with its date
  and revision, and marked it (`sync_enabled_via = 'migration'`, `sync_enabled_by` = the linker).
  The text they agreed to promised that turning their sync off deletes the index of every mailbox
  they linked; until an owner or an admin confirms the team's consent at the current revision —
  which replaces the mark — that promise holds: the linker withdrawing their consent, or being
  disabled or deleted on the instance, stops the mailbox and deletes its index in the same
  transaction (`store.StopBoundTx`), and tells its other readers. While someone else reads such a mailbox, disabling or
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
| switch sync on or off | a team mailbox: its owners and admins, signed in (the team's consent), and off for a removed one, its owners; a personal one: never (its person's own consent decides), but its person lifts a recovery's hold | never | operator mailbox, unrestricted admin key |
| act | `act`, actions consent, the mailbox syncing | `act`, `write` scope (under its key terms; its creator's actions consent only as the Consents table says), the mailbox syncing | operator mailbox, `write` scope, the mailbox syncing |
| send, read own send records | `send`, send consent | `send`, `send` scope, `MAIL_KEYS_MAY_SEND` (under its key terms; no person's consent) | operator mailbox, `send` scope |
| re-authorize | manage (owner or admin, or a member holding `manage`) | never | operator mailbox, `admin` scope |
| remove (repeating its id) | a team's: its owners (an admin, their own link that never connected); a personal one: its person | never | operator mailbox, `admin` scope |
| see the removed ones, recover one | a team's: its owners; a personal one: its person | never | operator mailbox, unrestricted admin key |
| delete a removed one for good (repeating its id) | a team's: its owners; a personal one: its person | never | any removed mailbox, unrestricted admin key |
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
| remove a mailbox | yes | only their own link that never connected | no | operator mailboxes only |
| see the removed mailboxes, recover one | yes | no | no | sees every workspace's; recovers operator mailboxes only |
| delete a removed mailbox for good | yes | no | no | any |
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
| **Last owner** | demoting, disabling or removing the last active owner of a local team, or that owner leaving | promote another member first |
| **Last reader** | taking `read` from the last person who can read a team mailbox — revoking it, disabling or removing their membership, leaving, closing their account without `force` | give another member `read` first (an owner or an admin who reads it can), or remove the mailbox |
| **Personal** | any change to a personal workspace's membership; an invite into one; a grant change on one of its mailboxes | |
| **Operator** | a membership, grant or invite in the operator workspace | |

The **last-reader rule** is Mailie's form of Wappie's `requireRemainingReader`: a team mailbox with
at least one reader is never left with none. A reader is an active member, active on the instance,
holding `read`, and on a mailbox that has a key a grant at its current epoch; keys, disabled
members, people disabled or suspended on the instance, a member waiting for the key and a role on
its own never count ("ownership is not a recovery path"), and taking a waiting member's flag leaves the readers as they were. The reset of a
person's account key (`mailserver user password --bootstrap`) deletes their grants and keeps their
flags, so it refuses, without `--force`, only where they are the last reader of a team mailbox
**that has a key**; on one without, they keep reading by the flag. After a forced reset, a team
mailbox left with no reader stops syncing. Removing the mailbox is not guarded, nor is a role change; a removed
mailbox has no reader while it is kept, and protects none. The database has one
writer, so two changes that would each leave one reader are ordered, and the second is refused.
The refusal says: "that is the last person who can read a mailbox of the workspace; give another
member read on it first, or remove the mailbox".

On the instance, `user disable` and `user delete` also refuse, unless `force`, a person who is the
last active owner of a local team with other active members; the last reader of a team mailbox of a
team that outlives them — every team but a local one whose only member they are: a team another
member remains in, whatever their status, or a team whose source is not local, which never goes
with anyone here — since nobody could read the mailbox again; or the linker a migrated team consent
is still bound to while someone else reads that mailbox. A team whose source is not local has no
last owner here: its owners are decided where it comes from, so no count of its members here marks
one (`last_owner`) or holds back closing one. The instance's own last-owner rule stays as
it is. With `force`, a team can be left without an owner (the operator assigns one with `mailserver
member role`), a team mailbox without a reader, which then syncs nothing and is marked
`no_reader`, and a bound mailbox stopped with its index deleted.
A suspension (see [People on the instance](#people-on-the-instance)) is refused by none of these,
and while suspended a person counts in none of them: not as another owner, another reader, or the
instance's other active owner. Disabling or deleting a suspended person here asks them again, as for
anyone active, but for the instance's last owner: a team mailbox they would be the only reader of
once resumed counts as one they read, since it waits for them and closing them would leave it to
nobody for good.
Deleting a person removes the mailboxes of their personal workspace and every local team whose only
member they were, with its mailboxes, removed ones included, for good and at once, through the account registry's usual clean-up (workers,
cached credentials, consent attempts, compaction); a team mailbox of a team others are in stays,
and so does every mailbox of a team whose source is not local, with the team, even when the person
was its last member here. One rule decides which teams go with a person, for the deletion of the
workspaces and of their mailboxes and for the last-reader test alike
(`workspace.GoesWithPersonSQL`).

**What strands a mailbox.** A change no protection refuses may leave a team mailbox with nobody who
reads it, would read it once resumed, or would read it but for a hold
(`workspace.StrandCandidatesTx`, asked before the change): a person disabled or deleted with `force`,
or reset with `--force` on a mailbox that has a key, or one whose own held membership kept a mailbox
nobody else reads, which no protection asks about; and, under a source that mirrors an external
directory, a membership it ends, the generation confirmed with memberships held, and a person it
closes, forgotten ([Following an external directory](#following-an-external-directory)). Each is
journaled stranded in the same transaction, naming the person it lost and why (`disabled`,
`deleted`, `reset`, `membership_ended`, `forgotten`), by whoever did it, and tells nobody in the
console. Never: a suspension (what the person reads waits for them), a role change, closing a team,
removing a mailbox, or putting a membership on hold. What an external directory decides is refused
by none of the protections above: it applies in a closed team too, to a person who is not active,
and to the last owner and the last reader, whom only its hold, while its generation is not
confirmed, keeps for a while.

### Taking over a link

Gone with the linker. Phase 2 kept a team mailbox syncing under its linker's consent, so the linker
was protected and `POST /v1/accounts/{id}/take-over` moved the link to someone else; a team mailbox
is the team's now, and that route answers `404`.

## Errors

The seven codes do not change.

| Situation | Code |
|---|---|
| a mailbox the caller neither holds a grant on nor manages by their role, another workspace's mailbox, a workspace the caller is not an active member of; a grant's recipient who is not an active member of the mailbox's workspace; a closed team's mailbox, for every person and key | `404 not_found`, never `403` |
| a mailbox the caller sees, without the flag the operation needs (a member waiting for the key reads nothing); a role that may not do this (a member listing members, leaving, granting, creating a key); a key on an administration, key or mailbox-key route; a key's send where keys may not send; a giver of `read` or of the key, or a first key's writer, who does not read the mailbox now; a step-up more than ten minutes old; a team mailbox's new key; a team, or a team mailbox the caller would otherwise see, in an edition that withholds teams | `403 not_authorized` |
| a protection; a platform-sourced workspace changed locally, or closed by the operator; a closed team used or changed (its members, invites, keys, grants, name, a mailbox linked into it); a missing actions or send consent; a grant at another epoch than the mailbox key's current one, or a new key at another than the next; a grant that already exists; a grant that names another account public key than its recipient's now (they were reset since the console read them); a first key for a mailbox that has one, or whose grants are not exactly its readers' with an account key; a namespace another mailbox uses; a grant for a person without an account key, or the key for one who holds no `read`; a person without an account key linking a mailbox | `409 conflict` |
| a personal or the operator workspace as the target of a member, grant or invite operation, or closed; an `act` without `read`; `manage` for an owner or an admin; a removal without its id repeated; a consent to another revision than the current one; a key's flag its scope does not allow; `POST /v1/me/apikeys`; a public key, namespace, grant or epoch outside its shape, or a grant at odds with the epoch its request names; a grant without the `public_key` it was sealed to, or a `public_key` without a grant; a person's link without its key, or an instance key's with one; `read` on a mailbox that has a key, to a person with an account key, without their grant; a grant with a change that gives no `read` | `400 bad_request` |

What an extension that follows an external directory calls answers in the same seven codes, each
keeping its cause for `errors.Is`: `409` for a server whose workspaces are local
(`ErrNotFollowing`), a document or a confirmation of another generation (`ErrDirectoryGeneration`),
a cursor or a closing older than what was applied (`ErrDirectoryMoved`), a workspace whose kind
changed (`ErrKindChanged`) or that is this server's own or another person's (`ErrRepointed`), a
person released while here (`ErrPersonStillHere`) and a person forgotten on a document that does not
close them (`ErrNotClosed`); `404` for an identity that signs in nobody (`ErrNoSuchPerson`); `400`
for a document outside its shape; and `500`, "upstream: …", with `Retry-After: 10`, for a new person
signing in without their document (`ErrDirectoryNeeded`). A snapshot larger than its bounds is
`409`, and one of a workspace that is not from the directory's source `404`.

**Freshness.** An act the freshness hook could not confirm with the directory is `500`, "upstream:
this server could not confirm with the directory its workspaces come from that you may do this; try
again shortly", with `Retry-After: 30` and no reason on the wire (the API logs it as any internal
error: accepted noise during an outage); an answer the hook gives as a `*service.Error` reaches the
caller as it is, and one that ended the actor's place is refused by the act's own re-check with the
act's own code (`404` for a workspace or mailbox no longer theirs, `401` for a session it ended).

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
removed from the workspace (a carried-over key loses that workspace's mailboxes), when they are
disabled, suspended or deleted on the instance, and, in every workspace, when they recover their
account with their recovery code or complete a reset invitation ([`key-scheme.md`](key-scheme.md)
sections 12.4 and 12.6): whoever held their password, their recovery code or a session of theirs
may have made one, and a key outlives a session. The console's forms and `mailserver user
password` say so before. Changing the password, replacing the recovery code and signing out
everywhere keep them, and so do demoting them and disabling their membership, as Wappie does.
Deleting a person deletes the keys of their personal workspace and of the teams they were alone
in, with those workspaces, and blanks their name on the keys they created in a team that stays,
which stay revoked as the team's record.

**Why a key stopped** (migration 0017). Every write that revokes a key records why, once, in
`api_keys.stop_reason`, and the listings answer it as `stop_reason` on a revoked key (absent on a
live one): `manual` (the operator, an owner or an admin of its workspace, or its creator revoked
it), `creator_left` (its creator left the workspace or was removed from it), `creator_disabled`,
`creator_suspended` (by an extension of the daemon), `creator_deleted`, `account_recovered` (its
creator recovered their account, with their recovery code or a reset invitation, which stops every
key they had created; or an extension stopped the keys created before a recovery made elsewhere),
`restriction_emptied` (the last mailbox it could reach went: an operator key restricted to
mailboxes that are all gone, or a carried-over key that lost its last one) and `migration`
(reserved). A carried-over key revoked
in, or losing with its creator, the last workspace it held mailboxes in says `manual` or
`creator_left`, not `restriction_emptied`. The schema holds it: a key that stops without a reason
is refused, a reason is only on a revoked key, and it never changes. A key revoked before 0017
says none. `mailserver apikey list` prints it after `revoked`. An extension stops a person's keys
created before a moment with `Service.RevokeKeysCreatedBefore`, naming the reason; no route does.

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
  removed, a mailbox linked or removed, a person disabled, deleted, suspended or resumed, a team
  closed or reopened, a recovery or a reset (which stop the person's keys), a key created, given or
  taken a mailbox, or revoked. A role never gives `read`, so a role change
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
- **Notices.** A person's stream also says, at the same moments, when they were given a notice
  since it last looked (`Stream.NewNotices`, which reads their notices again only when the service's
  notice epoch moved): `event: notice`, with no `id` and `{}` for data, after any `access` it
  writes, and even on a stream that ends then, since the notice says why it lost a mailbox. The
  console reads them with `GET /v1/me/notices`. What the person had when the stream opened is never
  announced, and a key has none ([Removed mailboxes](#removed-mailboxes)).
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
| `GET /v1/workspaces` | read | `[Workspace]`: a person's active memberships; an instance key the operator workspace; the operator every workspace; a closed team with its `closed_at`; a team marked `withheld` in an edition that withholds teams |
| `POST /v1/workspaces` | session; operator | `{name}` (the operator adds `owner_email`) → `Workspace` (201), a team, `local` source only |
| `PATCH /v1/workspaces/{id}` | team owner or admin; operator | `{name}` → `Workspace` |
| `POST /v1/workspaces/{id}/close` | operator, a local team only | `Workspace`, with `closed_at`: nothing in the team is used until it reopens; closing a closed team changes nothing |
| `POST /v1/workspaces/{id}/reopen` | operator, a local team only | `Workspace`, as it was before it closed |
| `GET /v1/workspaces/{id}/members` | owner or admin; operator (a member: `403`) | `[Member]` |
| `PATCH /v1/workspaces/{id}/members/{user_id}` | see the table; operator | `{role?, status?}` → `Member` |
| `DELETE /v1/workspaces/{id}/members/{user_id}` | see the table (an owner themselves while another owner remains); operator | `204` |
| `GET /v1/workspaces/{id}/invites` | owner or admin; operator | `[Invite]`, pending only |
| `POST /v1/workspaces/{id}/invites` | owner or admin; operator | `{email, role}` → `Invite` with its `url` (201) |
| `DELETE /v1/workspaces/{id}/invites/{invite_id}` | owner or admin; operator | `204`; the unused invite is deleted |
| `POST /v1/auth/invites/accept` | session | `{invite}` → `Workspace`: joins with the invite's role |
| `GET /v1/workspaces/{id}/access` | owner or admin; operator (a member: `403`) | `[MailboxAccess]`, every mailbox of the workspace |
| `PUT /v1/accounts/{id}/access/{user_id}` | see [the grant rules](#who-may-change-a-grant); session; operator for `manage` | `{read, act, send, manage, grant?, public_key?}`, all four flags required, not all false → `Grant`; `grant` with `read` given on a mailbox that has a key to a person with an account key, beside the `public_key` it was sealed to, which must be theirs now |
| `DELETE /v1/accounts/{id}/access/{user_id}?flags=` | owner or admin, their own flags included; operator | `204`; `flags` (comma-separated `read`, `act`, `send`, `manage`) names what goes, every flag without it; taking `read` deletes the person's grants on the mailbox |
| `GET /v1/accounts/{id}/mailbox-key` | session holding `read` on it (a person who sees only its card: `403`) | `MailboxKeyState`: the key pair, the caller's own grant, and whom they may seal to or who may seal to them |
| `POST /v1/accounts/{id}/mailbox-key` | session reading it by the flag, with an account key; fresh step-up | `{public_key, namespace, grants: [{user_id, grant, public_key}]}` → `MailboxKeyPair` (201), the first key of a mailbox without one; each grant names the account key it was sealed to, which must be its person's now |
| `PUT /v1/accounts/{id}/mailbox-key` | session, the personal mailbox's person; fresh step-up | `{epoch, public_key, grant}` → `MailboxKeyPair`, the next key; older grants deleted |
| `PUT /v1/accounts/{id}/grants/{user_id}` | session reading it; fresh step-up | `{epoch, grant, public_key}` → `SealedGrant`: the key, to a member who holds `read` without it, sealed to the account key `public_key` names, which must be theirs now |
| `GET /v1/workspaces/{id}/apikeys` | owner or admin, signed in | `[WorkspaceKey]`: every key of the workspace and the carried-over keys holding one of its mailboxes, revoked and expired ones too, live first |
| `POST /v1/workspaces/{id}/apikeys` | owner or admin, signed in | `{name, scope, ttl_days, terms_version, mailboxes: [{account_id, read, act, send}]}` → `{key, …WorkspaceKey}` (201), the secret shown once |
| `DELETE /v1/workspaces/{id}/apikeys/{prefix}` | owner or admin, signed in | `204`; a carried-over key loses the workspace's mailboxes instead |
| `PUT /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account_id}` | owner or admin, signed in; `read` only from one who reads the mailbox | `{read, act, send}`, all three required, not all false → `KeyMailbox` |
| `DELETE /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account_id}` | owner or admin, signed in | `204`: the mailbox is out of the key |
| `GET /v1/workspaces/{id}/apikeys/{prefix}/sends` | owner or admin, signed in | `[SendStatus]`, the key's sends from the workspace's mailboxes, newest first (at most 200) |
| `GET /v1/workspaces/{id}/removed-mailboxes` | a personal workspace's person, a team's active owners, signed in (an admin or a member: `403`); the operator, any workspace's | `[RemovedMailbox]`, the oldest removal first |
| `POST /v1/accounts/{id}/recover` | its owners, as above; the operator, an operator mailbox only (a person's or a team's: `403`) | `Account`, live again, `sync.held` until someone who may turns it on; `409` past its 30 days, in a closed team, or for a team mailbox nobody who could read it is left to read; `404` for everyone else |
| `POST /v1/accounts/{id}/purge` | its owners, as above; the operator, any workspace's | `{confirm}`, the id repeated, or `400` and nothing deleted → `204` (150 s): deleted for good; a live mailbox `409` |
| `GET /v1/me/notices` | session (a key: `403`) | `[PersonNotice]`, the newest first |
| `DELETE /v1/me/notices/{id}` | session | `204`; one the person does not have, `404` |
| `GET /v1/me/apikeys` | session | `[WorkspaceKey]`: the keys the person created, in every workspace |
| `DELETE /v1/me/apikeys/{prefix}` | session | `204`: revokes a key the person created |
| `POST /v1/me/apikeys` | session | `400`: keys are created in a workspace |

Gone: `POST /v1/accounts/{id}/take-over` (`404`; see [Taking over a link](#taking-over-a-link)).

Changed routes:

| Route | Change |
|---|---|
| `POST /v1/accounts` | optional `workspace_id`; who may link where, as [above](#mailboxes); into a team, optional `sync_consent_version` (the current sync text: the team's consent, given with the link); from a person, `public_key`, `namespace` and `grant` (the mailbox's first key, phase 3), with a fresh step-up |
| `PUT /v1/accounts/{id}/access/{user_id}` | optional `grant` and `public_key`: required together with `read` given on a mailbox that has a key to a person with an account key, refused otherwise; `public_key` is the key the grant was sealed to, `conflict` unless it is the person's now (phase 3) |
| `GET /v1/accounts`, `GET /v1/accounts/{id}`, `GET /v1/accounts/{id}/sync` | a team's owners and admins see the card of each of its mailboxes |
| `GET /v1/accounts`, `GET /v1/messages`, `GET /v1/me/storage`, `GET /v1/events`, `GET /v1/events/wait` | optional `?workspace=` |
| `DELETE /v1/accounts/{id}?confirm=<id>` | the id repeated, or `400` and nothing removed; a team's owners (an admin, their own link that never connected), a personal mailbox's person, the operator for its own; kept 30 days, or deleted at once when it never connected; answers `Removal` (`200`, `204` before 0020) |
| `POST /v1/accounts/{id}/oauth/start`, `POST /v1/accounts/oauth/callback` | manage (callback: the flow is still only its starter's) |
| `PUT /v1/accounts/{id}/sync` | `{enabled, version}`: a team mailbox's consent, from its owners and admins signed in (on: `version` the current sync text; off: deletes its index for everyone); an operator mailbox's switch, from the operator, with no `version`; on lifts a recovery's hold too; for a personal mailbox, `{"enabled": true}` from its person, which only lifts the hold, and `400` otherwise; a removed team mailbox's owners turn it off; `409` to turn on a team mailbox nobody can read. Answers `AccountSync`, with `held` and `held_since` while a recovery holds it |
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
// the operator's listing has no role or status, and adds "members" and "mailboxes" counts;
// a closed team adds "closed_at", when it closed; in an edition that withholds teams, every team
// adds "withheld": true

// Member ("person_disabled": true only for a person switched off on the instance, disabled here or
// suspended; "person_suspended": true only for a suspended one; last_reader_of
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
          "version": "2026-10-open-sync-4", "current": true},
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
// server allowing; a revoked key adds revoked_at and stop_reason, why it stopped, absent on a key
// revoked before 0017)
{"prefix": "0a1b2c3d", "name": "Claude Code", "scope": "read", "workspace_id": "wsp_…",
 "mailboxes": [{"account_id": "acc_…", "workspace_id": "wsp_…", "read": true, "act": false,
                "send": false, "granted_by": "usr_…", "updated_at": 1790000000}],
 "created_by": "usr_…", "created_at": 1790000000, "expires_at": 1797776000,
 "last_used_at": 1790003600, "live": true, "terms_version": "2026-10-open-api-keys-3",
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

// Removal (DELETE /v1/accounts/{id}): purge_after, until when it is kept and recovered, absent
// when purged, a link that never connected deleted at once
{"account_id": "acc_…", "purged": false, "purge_after": 1792592000}

// RemovedMailbox (removed_by "usr_…", "key:<prefix>" or "cli", absent once that person is deleted;
// index_kept, an index a recovery catches up from, false once its sync was turned off; readers,
// who would read it again once recovered: a team mailbox with none is deleted and linked again)
{"id": "acc_…", "workspace_id": "wsp_…", "email": "billing@example.org", "provider": "imap",
 "removed_at": 1790000000, "removed_by": "usr_…", "purge_after": 1792592000,
 "index_kept": true, "readers": 2}

// PersonNotice (kind "removed" or "sync_off"; state where the mailbox stands now, "removed",
// "deleted" or "live"; mailbox its address while it is kept, its masked hint once deleted;
// workspace_name absent for a personal workspace and once the team is gone; recoverable_until only
// while it is removed)
{"id": "mbe_…", "kind": "removed", "workspace_id": "wsp_…", "workspace_name": "Support",
 "account_id": "acc_…", "mailbox": "billing@example.org", "at": 1790000000, "state": "removed",
 "recoverable_until": 1792592000}

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
mailserver workspace close  --workspace ID     # a local team: nothing in it is used until it reopens
mailserver workspace reopen --workspace ID
mailserver member list   --workspace ID
mailserver member role   --workspace ID --email EMAIL --role owner|admin|member
mailserver member remove --workspace ID --email EMAIL
mailserver access list   --workspace ID
mailserver access grant  --account ID --email EMAIL --manage
mailserver access revoke --account ID --email EMAIL [--read] [--act] [--send] [--manage]   # none: every flag
mailserver user invite --email EMAIL [--role owner|member]                      # an instance invite
mailserver user invite --email EMAIL --workspace ID [--role owner|admin|member] # a team invite
mailserver account remove  ID [--yes]           # an operator mailbox: kept 30 days, recoverable
mailserver account removed [--workspace ID]     # the operator workspace's by default; any workspace's
mailserver account recover ID                   # an operator mailbox, its sync held until `account sync ID on`
mailserver account purge   ID [--yes]           # a removed mailbox of any workspace, deleted for good
```

- `workspace close` and `reopen` take a local team only; `workspace list` notes a closed one
  (`closed since …`). See [Closed teams](#closed-teams).
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
- `account remove` says until when the mailbox is kept, or that one that never connected was
  deleted at once. `account removed` lists a workspace's removed mailboxes (the operator workspace's
  without `--workspace`), with who removed each, until when it is kept, whether its index is kept
  and how many people would read it again; `account recover` recovers an operator mailbox only (a
  person's or a team's is its owners' to recover, in the console), whose sync `account sync ID on`
  then switches on again; `account purge` deletes a removed mailbox of any workspace for good, after
  asking (`--yes` does not), to erase one when its person asks. See
  [Removed mailboxes](#removed-mailboxes).
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
	// for the source that mirrors an external directory, which creates nothing of its own.
	Changeable() error
	// PersonCreatedTx runs inside the transaction that creates a person. The local source
	// creates their personal workspace and its membership there; the other source does
	// nothing, since the directory's document, applied in that transaction, does.
	PersonCreatedTx(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error
}
```

- **Local** implements everything in this document.
- **Platform** (`workspace.Platform()`, stored as `platform`) mirrors an external directory that an
  extension of the daemon follows ([Following an external directory](#following-an-external-directory)):
  `Changeable` refuses (the service answers `409`, "workspaces are managed in the account console"),
  `PersonCreatedTx` creates nothing, and the directory's documents create and change every
  workspace and membership, the person's personal workspace included, through the mirror
  (`internal/workspace/mirror.go`). A person signing in for the first time is created only with
  their document, which names that workspace. A row's own `source` is checked too: a `platform`
  workspace is never changed locally, whatever the configured source.
- **Selection** is `app.Options.WorkspaceSource`, set only by a binary that embeds `internal/app`;
  `nil` is local. There is no environment variable, and `serve` never sets it: nothing hosted is in
  the core.
- Unchanged in both: the operator workspace, linking mailboxes, grants, keys and consents, which
  are Mailie's whatever the source.
- **A team whose source is not local outlives its people.** Its workspace and memberships are sent
  from elsewhere, so nothing here deletes it: deleting a person removes their membership and
  grants, never the team nor its mailboxes, even when they were its last member here; the team
  stays, with its mailboxes, their consent and their index, until its own source removes it. Its
  owners are not decided here either, so no count of its members makes anyone its last owner
  (`last_owner` is never set there, and closing a person never waits on it). What stays protected
  is its mailboxes' last reader: closing the last person who reads one needs `force`, since that
  team outlives them as one with other members does ([Protections](#protections)).
- A row's `kind`, `source` and `person_id` never change (migration 0016's trigger): a team never
  moves between sources, and so never from one side of these rules to the other.
- At start the daemon gives a personal workspace to any person without one, as old as the person
  (`workspace.Repository.RepairPersonal`), under the local source only. Every person gets theirs in
  the transaction that creates them; only a binary from before 0008, run on a migrated database,
  creates one without (see [Going back](#going-back)).

### Editions that withhold teams

Whether team workspaces are offered at all is an edition's choice, not a source's:
`app.Options.TeamsWithheld` (`service.Deps.TeamsWithheld`), set only by a binary that embeds
`internal/app` for an edition that does not offer teams yet. There is no environment variable, and
`serve` never sets it: the self-hosted server offers teams, whatever its source. It holds whatever
the database holds, a restored one included, and whichever source the teams came from.

- **Nothing of a team is used**, by anyone. No team is created, and no mailbox is linked into one.
  Its mailboxes reach nobody, its keys included: no listing shows them (the visibility rule leaves
  every team's mailbox out, for a person and for a key), and one asked for by a person or a key who
  would otherwise see it is refused by name (`403`), as is a `?workspace=` naming the team; someone
  who would not see it is still told it does not exist (`404`). Nobody changes its members, invites
  or name, signs up through an invite into it or accepts one signed in (the invite stays unspent),
  creates, lists, changes or revokes its keys, gives or takes what anyone holds on its mailboxes,
  gives or withdraws its consent to sync, or writes, reads or supplies a mailbox key there (`403`,
  "this server does not offer team workspaces yet").
  A person's own keys (`GET /v1/me/apikeys`) leave a team's keys out, and the team mailboxes a
  carried-over key holds; revoking a team's key there is refused as through the team's routes.
  The operator still reads its members, its invites and who holds what, and changes none of it.
- **Its mailboxes sync nothing**: the store's rule for what may be indexed leaves every team's
  mailbox out in such an edition (`store.Options.TeamsWithheld`), so the engine never starts one,
  refreshes none of its tokens, and refuses any batch for one; each keeps its index, its consent and
  its readers, and catches up in an edition that offers teams.
- **It is listed, marked so**: `withheld: true` on the team in `GET /v1/workspaces`, for its
  members, its keys and the operator, so a client shows a notice instead of what the team holds.
- **Nothing of it is lost**: withholding deletes and changes nothing, and stops no key; an edition
  that offers teams finds every team as it was. Personal workspaces and the operator's are as they
  were.

## Following an external directory

An extension of the daemon, in a binary that embeds it, may follow an external directory: a service
elsewhere that says, person by person, whether they are active and which of its workspaces they
belong to, with which role, and whose workspaces this server mirrors under a source that is not the
local one ([The workspace source](#the-workspace-source)). What its documents do to people,
workspaces and memberships is `internal/service`'s, in one write each. What the schema keeps for it
is here (migrations 0022 and 0023); a server that follows no directory, which is every self-hosted
one, leaves all of it empty.

- **A directory** (`directories`, one row per issuer, never deleted): the generation of its data
  the documents applied belong to, whether it confirmed that generation, how many times it was
  reset here (`reset`: a new generation, or the directory saying it forgot where its feed stood),
  how far its feed was read (`cursor`) and up to when it says it published every change
  (`published_through_us`, microseconds). One row, so a generation and its cursor move together;
  the cursor's write is a compare and set on the generation and the reset
  (`store.SetDirectoryCursorTx`), so an answer read before a reset never moves the feed, and the
  schema lets a generation, a cursor, a confirmation and what was published go back only with a
  reset, which never goes back (`directories_reset`). A reset (`store.ResetDirectoryTx`) starts the
  directory again at a generation, unconfirmed, its cursor and what it published forgotten.
- **What it says of a person** (`directory_people`, per issuer and person): their subject there,
  the generation and version of the document last applied and the reset that version counts
  under, their status, and the moments it disabled them, revoked their sessions and revoked their
  credentials, in microseconds, each kept as the highest ever seen, across resets too (the schema
  lowers none). The row is keyed by the person, not by the subject, and outlives their deletion
  here, marked `gone_at` in the transaction that deletes them, until an extension releases them
  (`store.ReleaseDirectoryPersonTx`) or 90 days pass: until then the journal keeps their name, and
  the same subject signing in again meanwhile is a new person with a row of their own, so a
  subject is unique only among the rows of people still here
  ([conflict 29](#conflicts-and-resolutions)).
- **Versions fall only with a recorded reset.** A person's document version falls only to 0, and
  only while its `reset` rises to the one its directory recorded; a workspace's version of its name
  and closure (`workspaces.source_version`) only to 0, and only while its `source_reset` rises to
  the sum of every directory's resets. A reset writes all of them in its one transaction, and a
  stray zeroing is refused unless a reset was recorded first. A local workspace has neither column
  (a CHECK), and a workspace from another source is written at the current sum.
- **A held membership** (`workspace_members.held_at`): one a directory ended while its generation
  was not confirmed, and whose end would leave a team mailbox with nobody who reads it, would read
  it once resumed, or would read it but for another hold. It is disabled, so its person reads
  nothing there and counts as no reader, and keeps its flags and sealed grants until a document
  lists it again or the generation is confirmed, which ends it (key scheme, section 12.13, "Being
  held"). Only a team's membership from another source is held, and never when written (the
  schema). `store.HeldReaderSQL` says whom a hold keeps a mailbox for; it decides no access.
- **An extension's own state** (`extension_state`): a few keys and values per extension, a value of
  a mebibyte at most, kept beside the data it describes so that a restore takes both back together
  (`store.ExtensionState`, `ExtensionStateKeys`, `WriteExtensionState`).
- **What changed** (`workspace_changes`, migration 0023): one row per workspace from another
  source, with the seq of the last change to anything a snapshot of who holds what there shows —
  its mailboxes (linked, deleted, their state, name, removal, sync consent and hold), the flags,
  mailbox keys and sealed grants on them, its members (their role, status and hold) and those
  members' own status, suspension, account key and sync consent, its name and closure, and its
  journal. The schema's triggers write it in the transaction of the change; a seq is never reused
  and a workspace's only rises; a deleted workspace's last mark stays, so an extension learns it is
  gone. An extension reads the marks above the last it saw (`store.WorkspaceChanges`). A local
  workspace is never marked.
- **The journal as an outbox.** An extension reads a workspace's journal in order, from a cursor
  (`mailbox_events_workspace`), and reports it elsewhere: the strandings, with the person each
  lost, and a link that never connected deleted in such a workspace
  ([Removed mailboxes](#removed-mailboxes)).

### What a document does

A document is what the directory says of one person now: its issuer and subject (the identity that
signs them in here), the generation of its data it belongs to, a version, a status (`active`,
`disabled` or `closed`), the moments it last disabled them and revoked their sessions and their
credentials (microseconds; none for never), and the workspaces they belong to: their personal
workspace first, as its owner, and the teams, each with their role there and the team's own
version of its name and closure. The service applies it in one write
(`service.ApplyDirectoryDocument`), only under the source that mirrors a directory (`409`
otherwise), and for no transport:

1. **Whose.** The person the identity signs in (`404` for nobody). A document never creates a
   person but at a first sign-in (below).
2. **Which generation.** The directory is followed at the generation of the first document applied
   for it; a document of another generation is refused (`409`): the extension resets the
   directory first.
3. **Only newer.** A version no higher than the last one applied for the person changes nothing.
4. **What it revoked, when it rose** past what was kept, at microseconds: a disable rising, or a
   status `disabled` or `closed` for a person not suspended, suspends them (a person disabled here
   stays as they are: a disable here stands over a directory); a session revocation ends every
   session that started at or before it (a session starts at a whole second, so one started within
   that second ends too, never one started before it spared); a credentials revocation stops every
   key they created before it and the extension's margin, in every workspace
   (`account_recovered`). The same moment again ends nothing.
5. **A closed person** stops there, suspended: their memberships and their deletion wait for an
   answer from the directory that confirms it (`ForgetExternalPerson`, below).
6. **The workspaces are checked before anything of them is written**: one personal workspace,
   first, its person its owner, never closed; every id one a directory may send — 1 to 64 of
   letters, digits, `_`, `.` and `-`, never `wsp_…`, so never one of this server's — once; each
   team's role and name; an id already mirrored keeps its kind (`409`, `ErrKindChanged`), its
   source and, personal, its person (`409`, `ErrRepointed`). A refusal still commits what step 4
   revoked and the person's status, never the version, and is returned after the commit.
7. **The personal workspace** is the directory's: one a person already has under another id — the
   local one signing up made, before the directory was followed — is replaced while it holds no
   mailbox, live or removed, and no live key (its stopped keys and its membership go with it), and
   kept otherwise, the replacement waiting for a later document and the rest applying; the daemon
   names, at start, the people a document would not replace this way
   (`service.PersonalReplacementBlocks`).
8. **Each team** is mirrored with the directory's id, the source's name and its name and closure;
   one already there takes them only from a higher version than the one it took last, so an older
   document never undoes a newer one, and from any after a reset. A team the directory closes is
   closed as the operator closes a local one ([Closed teams](#closed-teams)).
9. **Each membership listed** is written: a new one whatever its person's standing (a suspended
   person is a member, reading nothing until resumed) and in a closed team too; a role changed as
   it is changed here (a demotion to member ends the consent attempts they started on the team's
   mailboxes, any change expires the invites they made there, and a key they created stays, the
   workspace's); a held one made active again, its flags and grants counting again. **Each
   membership the document omits**, of a team from another source, ends, refused by nothing — not
   the last owner, not the last reader, not a closed team: it takes the person's grants there,
   their consent attempts, the team's invites for their address, the invites they made there and
   the keys they created there (`creator_left`), and journals stranded each mailbox it leaves with
   nobody who reads it, would read it once resumed or would read it but for a hold
   (`membership_ended`, by `directory`). While the directory's generation is not confirmed, a
   membership whose end would strand a mailbox is **held** instead: disabled, its flags and sealed
   grants kept, unusable, what a disable ends ended; a document that lists it again releases it,
   and confirming the generation ends it.
10. **A resume** only by an active document that knows of the disable that suspended them: its
    disable no older than the one kept. One that does not (a reset let an older document apply)
    keeps the suspension, and says so. A disable and an enable both missed are a suspension and a
    resume in the same write: what the disable ends ends.
11. **What it said** is recorded: its version, generation and status, and the highest of each
    revocation.

Once the write commits, event streams read access again, the sync engine hears of the mailboxes of
every team it changed and of the person's own, and a suspended person's consent attempts stop.

**A first sign-in** (`service.ExternalSignIn.Directory`): for a person who exists the document is
applied first, in a write of its own, so one it suspends is refused as any disabled person is; for a
new person it is applied in the transaction that creates them (`auth.ExternalSignIn.PersonCreatedTx`,
right after their identity is linked), which it rolls back when it suspends them or does not apply:
nobody is created without the personal workspace the directory names, nor disabled. That step goes
with every sign-in that carries a document, the person's existing or not: one deleted or forgotten
after the first write, whose identity a second sign-in pinned again meanwhile, is a new person to the
sign-in's own write, and is created with the document or not at all. Without a
document, a new person under the source that mirrors a directory is not created (`internal`,
"upstream: …", `Retry-After: 10`): the extension asks the directory and tries again. Under the
local source a document is refused (`409`).

### Generations, resets and holds

- **A reset** (`service.ResetDirectory`) starts the directory again at a generation, in one
  transaction: unconfirmed, its cursor and what it published forgotten, every person's version and
  every team's version set back to 0 under the reset it records, what it revoked kept. An extension
  resets when the directory says it forgot where its feed stood, whatever the generation, and on a
  new generation; a directory never followed is created by it. The next document of each person
  then applies whatever its version, and the next state of each team is taken from it.
- **The feed's cursor** (`service.SetDirectoryCursor`) is a compare and set on the generation and
  the reset it was read under, never created by it, never going back.
- **Confirming the generation** (`service.ConfirmDirectoryGeneration`), when the directory says it
  is final, ends every held membership, one after another in the transaction, each journaling the
  mailboxes its hold kept that nobody else reads, would read once resumed or would read but for
  another hold: a mailbox two holds kept strands once, when the last of them ends, naming that
  person. Confirming again changes nothing.

### A person the directory closed

A closed document suspends the person (step 5). On an answer that confirms it — the document of
the directory's generation, closed, no older than the one applied — the extension forgets them
(`service.ForgetExternalPerson`): a deletion with force, refused by nothing, the instance's last
owner included, which takes everything `DeleteUser` takes — their personal workspace and its
mailboxes, their sessions, their identities, their memberships — and journals stranded
(`forgotten`, by `directory`), while they still exist, each team mailbox they alone read, would read
once resumed or would read but for a hold. Their directory row stays, gone, and with it their name
in the journal, until the extension has delivered what names them and releases them
(`service.ReleaseDirectoryPerson`, refused while they exist), or 90 days pass
(`service.SweepDirectory`, the daemon's housekeeping). The listings it maps them with:
`DirectoryPeople` (every person an identity of the issuer signs in now, with what was applied, and
every gone row), `DirectorySubjects` and `IdentityUser`.

### What an extension reads, and the freshness of an act

An extension reports a followed workspace to the directory it comes from, and asks it, before some
acts, whether the actor still stands where this server thinks; the core gives it reads that decide
nothing, and one question it asks. No transport calls any of them, and none takes a principal.

- **The journal as an outbox** (`service.MailboxJournal`): a workspace's entries after a seq, oldest
  first, 1 to 500 at a time, each with who read the mailbox then (`mailbox_event_readers`), its
  actor raw and the person behind it — the person themself for `usr_…`, a key's creator for
  `key:<prefix>` while the key's row is there, nobody for `cli`, `sweep` or `directory` — the
  person a stranding lost, its cause, and a removal's end (`purge_after`). The extension keeps its
  own cursor per workspace; the entries stay for their 90 days whatever it reads.
  `MailboxJournalHead` is the newest seq.
- **The followed workspaces** (`FollowedWorkspaces`: every workspace whose source is not the local
  one, with its kind, its person and whether it is closed) and **which changed**
  (`WorkspaceChanges`: the marks the schema writes, after a seq, with the head, read at one moment).
- **A snapshot** of who holds what on a followed workspace (`service.AccessSnapshot`, in one
  read-only transaction, `store.ReadTx`), at most 1000 mailboxes and 5000 people on one (`409`
  otherwise): every mailbox, live and removed until it is deleted for good, with its label and
  masked hint — never its address — and, for a live one, why it does not sync, the first that holds
  of one ordered table beside the sync rule (`store.PauseSQL`, empty exactly when it may sync):
  `team_closed`, `withheld`, `pending_auth`, `needs_reauth`, `error` (or disabled), `held`, then its
  consent — `person_off` or `consent_off` for a personal mailbox, `consent_off` for an operator's,
  `consent_off`, `person_off` (a consent bound to someone not active) or `no_reader` for a team's;
  each person who holds a flag or a sealed grant on it, with their stored flags, whether they read
  it now (computed without the team's closure: the pause carries it), wait for its key, have an
  account key, and hold a grant at its current epoch (`holds_key`: `store.LiveSQL` and
  `store.CurrentGrantSQL`, a suspended holder included); and, in a team, the people it cannot lose,
  each the only one who reads it. A removed mailbox is reported, by default, as it would be once
  recovered — who would read it, wait for its key and hold its key, and its kept last readers as
  protected — until it is deleted for good, as the directory asks of every product
  (`service.RemovedAsKept`); the other way (`RemovedAsNobody`, one switch, `Deps.RemovedInSnapshot`,
  which a binary that embeds the daemon sets through `app.Options.RemovedInSnapshot`, never `serve`)
  reports nobody. Either way a removed mailbox has no reader here and protects nobody in what this
  server refuses: only what the snapshot says follows the directory's rule
  ([conflict 31](#conflicts-and-resolutions)).
- **Its own state** (`app.Deps.State(name)`, over `extension_state`): a few keys of at most 256
  bytes and values of a mebibyte at most, per extension name (1 to 64 of `a-z`, `0-9`, `_`, `-`),
  written in one transaction each, kept in the database it describes, so that a restore takes it
  back with that data, never ahead of it.
- **The freshness of an act** (`service.Freshness`, `app.Options.Freshness`, never set by `serve`):
  before eleven acts of a person signed in, after the act's own read-only checks and outside every
  write, the service asks the extension, which may read the actor's standing from the directory and
  apply what it reads (a write: never asked inside one). The acts: giving read or flags, with a
  grant or without (`share_key` when a key is sealed to the person, `raise_flags` when a flag of
  theirs rises), supplying the key, a first key, a personal mailbox's new key (`share_key`, naming
  who the key is sealed to but the actor, or the actor alone for their own new key), raising what a
  workspace key holds (`raise_flags`), creating a key (`create_key`), linking a mailbox into a team
  (`link`), starting a team mailbox's consent again (`reauthorize`), switching a team mailbox's
  sync on or off (`team_sync`), an action on a team mailbox (`act`) and a send from one (`send`).
  It gates and never authorizes: the write that makes the act asks again who may, in its
  transaction — and starting a consent, an action and a send, which write nothing first, ask again
  at once — so an answer that ended the actor's membership, or disabled them, stops the act, which
  changes nothing. A `*service.Error` it returns is the caller's answer as it is; a context's end is
  passed on; any other error is `internal`, "upstream: …", with `Retry-After: 30`, and no reason on
  the wire. Never asked: reading, removing, recovering, deleting for good, taking access away,
  turning a removed mailbox's sync off, a key's own acts and the operator's.

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
member who held the flag without an account key (one who has not enrolled) waits for the key until
a reader supplies it once they enrol. Operator mailboxes stay without a key. The tests in
`internal/store/migrate_fourteen_test.go` hold every table's rows and columns as they were, both
new tables empty, and each refusal of the schema.

## Migration 0016

`0016_fixed_workspaces.sql`, an ordinary migration: one trigger, `workspaces_fixed`, which refuses
any update that changes a workspace's `kind`, `source` or `person_id`, and nothing else. No row of
a schema-15 database changes; nothing ever changed those columns, and a team's name still changes.
What deleting a person takes with them depends on a team's source (see
[The workspace source](#the-workspace-source)), so a source that could change would move a team
from one side of that rule to the other. The tests in `internal/store/migrate_sixteen_test.go`
hold every table's rows as they were and each refusal.

## Migration 0018

`0018_suspended_people.sql`, an ordinary migration: one column, `users.suspended_at`, the moment an
extension suspended the person, 0 otherwise, with a CHECK that a suspended person is disabled
(`suspended_at = 0 OR status = 'disabled'`). No row of a schema-17 database changes: every person
keeps their status, and a person disabled before stays disabled here. `status = 'disabled'` now
means "not active", disabled here or suspended, so every rule that reads `status = 'active'`
refuses a suspended person with nothing changed in it (see
[People on the instance](#people-on-the-instance)). The tests in
`internal/store/migrate_eighteen_test.go` hold every table's rows as they were and the CHECK's
refusals.

## Migration 0019

`0019_closed_teams.sql`, an ordinary migration: one column, `workspaces.closed_at`, 0 while a team
is open, with a CHECK that only a team closes, and the triggers that give nothing in a closed team
(`accounts`, `mailbox_access` and `key_access` inserts and flags going from 0 to 1,
`mailbox_keys`, `mailbox_grants`, `api_keys` and team `invites`). No row of a schema-18 database
changes: every team is open. See [Closed teams](#closed-teams). The tests in
`internal/store/migrate_nineteen_test.go` hold every table's rows and every trigger as they were,
and each refusal and what stays allowed.

## Migration 0020

`0020_removed_mailboxes.sql`, an ordinary migration: four columns on `accounts` (`removed_at`,
`removed_by`, `purge_after`, `sync_held_at`, all 0 or empty while a mailbox is live and not held),
the journal `mailbox_events` with `mailbox_event_readers`, and `notices`. Its triggers link every
mailbox live, keep a removal whole, 30 days at least, and fixed until a recovery, give nothing on a
removed mailbox (`mailbox_access`, `key_access`, `api_key_accounts`, `mailbox_keys` and
`mailbox_grants` inserts, and flags going from 0 to 1), write each journal entry once, and send a
notice only to a recorded reader, never to whoever acted. The credentials are not held: a token
refresh already under way records the refresh token the provider rotated, which a recovery needs.
No row of a schema-19 database changes: every mailbox is live. See
[Removed mailboxes](#removed-mailboxes). The tests in `internal/store/migrate_twenty_test.go` hold
every table's rows and every trigger as they were, and each refusal.

## Migration 0021

`0021_stranded_mailboxes.sql`, the second **rebuild** ([Rebuilding a table safely](#rebuilding-a-table-safely)):
`mailbox_events` gains the kind `stranded` and three columns, `cause`, `lost_user` and
`purge_after`, each held by a CHECK to the kinds that have it (a stranding always has a cause, a
sync turned off may say `bound_stopped`, only a stranding loses someone, only a removal has an end,
and always), and an index by workspace and seq. A CHECK constraint is added only by rebuilding a
table, so every row is copied with its seq, a removal recorded before taking its moment and 30
days as its end, exactly what it wrote then; `notices_to_readers`, whose body names the journal, is
dropped before the rename and written again byte for byte; the journal's AUTOINCREMENT high-water
mark is carried over as it was, and a journal never written keeps none. The trigger that writes an
entry once now keeps its cause and end too, and lets its lost person go only to nothing. Every row
keeps every value, and no other table changes. Back up before upgrading, as before any rebuild.
The tests in `internal/store/migrate_twentyone_test.go` hold every row, reader, notice and
high-water mark, the schema but for the journal's table, index and trigger, and each refusal.

## Migration 0022

`0022_directories.sql`, an ordinary migration: the tables `directories`, `directory_people` and
`extension_state`, the columns `workspaces.source_version` and `source_reset` and
`workspace_members.held_at`, all 0 on every row there was, and their triggers
([Following an external directory](#following-an-external-directory)). No row of a schema-21
database changes. The tests in `internal/store/migrate_twentytwo_test.go` hold every row and every
object as they were, and each refusal: a hold anywhere but in a team from another source, what a
directory revoked going down, a version falling without a recorded reset, a local workspace with a
source version, a directory's generation or cursor going back without a reset; and that a gone row
and a live one may share a subject.

## Migration 0023

`0023_workspace_changes.sql`, an ordinary migration: the table `workspace_changes` and the
triggers that mark a workspace from another source in the transaction of every change its snapshot
shows, each deleting the workspace's row and writing it again (never `INSERT OR REPLACE`, whose
clause an outer `INSERT OR IGNORE` would override), and marking on an update only when a value it
watches changes. The migration marks nothing, and no row of a schema-22 database changes. The tests
in `internal/store/migrate_twentythree_test.go` hold that, every change a snapshot shows marking its
workspace and only it, a person's deletion marking every workspace they were in through the
schema's cascade, a local workspace never marked, a mark only rising, and an outer `INSERT OR
IGNORE` still marking.

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
- Removed mailboxes: `TestOnlyAnOwnerRemovesATeamMailbox`;
  `TestAnAdminMayAbandonOnlyTheirOwnLinkThatNeverConnected`;
  `TestAMailboxThatNeverConnectedIsDeletedAtOnce`; `TestARemovedMailboxIsGoneForEveryUse`;
  `TestARemovedMailboxIsNeverDialledNorRefreshed`; `TestAGrantFinishingAfterRemovalStoresNothing`;
  `TestRemovingStopsAConsentAttemptAndItsRedirectFindsNothing`;
  `TestRemovingDeletesWhatKeysHeldAndNeverWidensAnInstanceKey`;
  `TestRelinkingARemovedAddressIsAConflict`; `TestReadersAreToldOnceAndTheActorIsNot`;
  `TestTurningOffATeamMailboxsSyncTellsItsOtherReaders`;
  `TestRecoveringIsAnOwnersWithinThirtyDays`;
  `TestARecoveredMailboxStaysOffUntilSomeoneWhoMayTurnsItOn`;
  `TestRecoveryRestoresQualifyingReadersButNoKeyAccess`;
  `TestDeletingForGoodIsForAnOwnerOrTheOperatorAndOnlyOfARemovedMailbox`;
  `TestTheSweepPurgesOnlyPastThirtyDays`, `TestHousekeepingPurgesRemovedMailboxesPastTheirThirtyDays`;
  `TestClosingAPersonPurgesTheirRemovedMailboxesAtOnce`; `TestAPurgedMailboxLeavesNothingOnDisk`;
  `TestARemovedMailboxNeverProtectsItsLastReader`;
  `TestADeletedMailboxIsNamedByItsHintOnlyWhileANoticeNeedsIt`.
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
- Why a key stopped, and recoveries: `TestEveryWayAKeyStopsSaysWhy`,
  `TestTheKeyListingsSayWhyAKeyStopped`, `TestOnlyAReasonAKeyStopsForStopsAPersonsKeys` and
  `TestASessionARecoveryEndedCreatesNoKey` (the service); `TestRevokingKeysCreatedBeforeATimeLeavesTheLaterOnes`,
  `TestARecoveryStopsTheKeysThePersonCreated`, `TestAResetStopsTheKeysThePersonCreated` and
  `TestAPasswordChangeKeepsThePersonsKeys` (`internal/auth`); `TestTheKeyListSaysWhyAKeyStopped`
  (`mailserver apikey list`); `TestMigrationSeventeenAddsTheStopReasonAndChangesNothingElse`,
  `TestTheSchemaRefusesAKeyThatStopsWithoutSayingWhy` and `TestWhyAKeyStoppedNeverChanges`
  (`internal/store`, migration 0017).
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
  `TestAPersonWithoutAPersonalWorkspaceGetsOneAtStart`;
  `TestAWorkspacesKindSourceAndPersonNeverChange` and
  `TestMigrationSixteenAddsOneTriggerAndChangesNothingElse` (`internal/store`, migration 0016);
  `TestDeletingAPersonNeverDeletesATeamWhoseSourceIsNotLocal`;
  `TestATeamFromAnotherSourceKeepsItsMailboxesWhenItsLastLocalMemberGoes`;
  `TestNoCountOfLocalMembersMakesALastOwnerOfATeamFromAnotherSource`;
  `TestTheLastReaderOfATeamFromAnotherSourceIsNotClosedWithoutForce`.
- Suspension: `TestSuspendingAPersonEndsTheirSessionsKeysAndInvitesAndKeepsTheirGrants`,
  `TestASuspendedPersonReadsNothingAndWaitsForNothing`, `TestASuspendedPersonsGrantIsNeverServed`,
  `TestNobodySuppliesTheKeyToASuspendedPerson`, `TestASuspendedPersonProtectsNothing`,
  `TestResumingRestoresReadingOnlyWithTheFlagAndAGrantAtTheCurrentEpoch`,
  `TestResumingBringsBackNoKeyAndNoSession`,
  `TestASuspendedPersonsPersonalMailboxesPauseAndCatchUpOnResume`,
  `TestATeamMailboxASuspendedPersonAloneReadsWaitsForThemRatherThanForNobody` and
  `TestATeamMailboxSyncingUnderASuspendedPersonsOwnConsentPausesAndKeepsItsIndex`,
  `TestClosingASuspendedPersonAsksForTheMailboxesTheyWouldReadOnceResumed`,
  `TestDisablingASuspendedPersonStopsTheirBoundConsentAndAsksTheProtectionsAgain` and
  `TestNeitherATeamsNorTheInstancesLastOwnerHoldsUpASuspension` (the service);
  `TestADisableHereStandsOverASuspension`, `TestSuspendingSomeoneDisabledHereChangesNothing` and
  `TestAKeyStoppedBySuspensionSaysSo` (`internal/auth`);
  `TestMigrationEighteenAddsTheSuspensionAndChangesNothingElse` and
  `TestTheSchemaRefusesASuspendedPersonWhoIsActive` (`internal/store`, migration 0018).
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

- Closed teams: `TestAClosedTeamsMailboxesReachNobodyOwnersIncluded`,
  `TestAClosedTeamsKeysReachNothingAndWorkAgainOnReopening`,
  `TestAClosedTeamSyncsNothingAndKeepsItsIndexAndConsent`,
  `TestNothingIsLinkedKeyedGrantedOrConsentedInAClosedTeam`, `TestTheSchemaGivesNothingInAClosedTeam`,
  `TestOnlyATeamCloses`, `TestClosingATeamEndsItsWaitingInvitesAndConsentAttempts`,
  `TestReopeningATeamRestoresAccessAndSync`, `TestTheCommandLineClosesOnlyALocalTeam` (with what
  `workspace list` says), `TestAClosedTeamIsListedAsClosed`, over REST
  `TestTheOperatorClosesATeamOverRESTAndNobodyReachesItsMailboxUntilItReopens`, at start
  `TestTheDaemonSaysAtStartWhichTeamsAreClosed` and `TestTheDaemonSaysNothingOfClosedTeamsWhereNoneIs`,
  and `TestMigrationNineteenAddsTheClosedTeamsAndChangesNothingElse` (`internal/store`, migration
  0019).

- Withheld teams: `TestAnEditionThatWithholdsTeamsRefusesEveryUseOfATeam`,
  `TestWithholdingTeamsLeavesPersonalWorkspacesAsTheyWere`, `TestAWithheldTeamIsListedWithItsNotice`,
  `TestAnEditionThatWithholdsTeamsSyncsNoTeamMailboxAndKeepsItsIndex`,
  `TestAnEditionThatWithholdsTeamsKeepsTheirKeysOutOfAPersonsOwnKeys` and
  `TestAnEditionThatWithholdsTeamsSignsNobodyUpIntoATeam` (the service);
  `TestABinaryThatEmbedsTheDaemonWithholdsTeamsThroughItsOptions` (the daemon, over REST) and
  `TestAnEditionThatWithholdsTeamsNeverStartsATeamMailboxsSync` (the daemon's engine); and
  `TestServeNeverWithholdsTeams`.

- Following an external directory, the storage (`internal/store`): migrations,
  `TestMigrationTwentyOneRebuildsTheJournalAndChangesNothingElse`,
  `TestMigrationTwentyOneOnAJournalNeverWrittenChangesNothingElse`,
  `TestTheRebuiltJournalNeverReusesASeq`, `TestMigrationTwentyTwoAddsTheDirectoryAndChangesNothingElse`
  and `TestMigrationTwentyThreeAddsTheChangeMarksAndChangesNothingElse`; the journal,
  `TestTheSchemaRecordsAStrandingWholeAndOnlyAsOne`, `TestTheSchemaNeverGivesANoticeOfAStranding`,
  `TestAStrandingIsWrittenOnceButItsLostPersonIsReleased`,
  `TestTheJournalKeepsNoLabelThatHoldsTheAddresssLocalPart` and
  `TestAConsentStoppedWithItsPersonIsJournaledAsBoundStopped`; holds,
  `TestAHeldMemberIsNeitherAReaderNorASuspendedReaderButAHeldOne` and
  `TestTheSchemaHoldsOnlyAMembershipOfATeamFromAnotherSource`; directories,
  `TestTheSchemaNeverLowersWhatADirectoryRevoked`,
  `TestTheSchemaLetsAVersionFallOnlyToZeroWithARecordedReset`,
  `TestTheSchemaGivesALocalWorkspaceNoSourceVersion`,
  `TestTheSchemaMovesADirectorysGenerationAndCursorBackOnlyWithAReset` and
  `TestAGoneRowAndALiveRowMayShareASubject`; `TestAnExtensionsStateTakesAMebibyteAndRefusesMore`;
  the change marks, `TestEveryChangeToWhatASnapshotShowsMarksItsWorkspace`,
  `TestDeletingAPersonMarksEveryWorkspaceTheyWereIn`, `TestALocalWorkspaceIsNeverMarked`,
  `TestAWorkspaceMarkOnlyRises` and `TestAnOuterInsertOrIgnoreStillMarks`. Beside them,
  `TestAStrandingIsJournaledNamingWhomItLostAndTellsNobody` (`internal/workspace`),
  `TestDeletingAPersonMarksTheirDirectoryRowGone` and
  `TestDeletingAnUnfollowedPersonForgetsThemAsTheLostReaderToo` (`internal/auth`), and
  `TestANeverConnectedLinkRemovedInAFollowedWorkspaceIsJournaledPurged` (`internal/account`).
- Following an external directory, its documents: the mirror (`internal/workspace`),
  `TestADocumentCreatesAWorkspaceWithTheDirectorysIDAndTheSourcesName`,
  `TestADirectoryIDNeverLooksLikeALocalOne`, `TestAPersonalWorkspaceFromADocumentReplacesAnEmptyLocalOne`,
  `TestAPersonalWorkspaceHoldingOnlyStoppedKeysIsReplaced`,
  `TestAPersonalWorkspaceHoldingAMailboxOrALiveKeyIsNotReplaced`, `TestADocumentNeverRepointsAWorkspace`,
  `TestAMembershipFromADocumentIsWrittenForASuspendedPersonAndInAClosedTeam`,
  `TestAMembershipEndedFromADocumentTakesGrantsInvitesAndKeysWithoutRefusingTheLastReaderOrOwner`,
  `TestAMembershipEndedFromADocumentStrandsWhatItsPersonAloneReadOrWouldRead`,
  `TestTheLastMemberLeavingATeamFromAnotherSourceLeavesTheTeamAndItsMailboxes`,
  `TestAHeldMembershipReadsNothingAndKeepsItsGrants`, `TestAMembershipListedAgainReleasesItsHold` and
  `TestEndingTheLastOfTwoHeldMembershipsStrandsTheMailboxOnceNamingThatPerson`; people
  (`internal/auth`), `TestRevokingSessionsStartedBeforeAMomentSparesLaterSeconds`,
  `TestANewPersonsCallbackRunsInTheTransactionThatCreatesThem`,
  `TestAForcedDisableJournalsEveryMailboxItStrands`,
  `TestAForcedResetJournalsEveryKeyedMailboxItStrandsWithItsIssuer`, `TestASuspensionStrandsNothing`
  and `TestAResumeInsideAnotherWriteNeverNestsOne`; the documents (`internal/service`),
  `TestADirectoryIsAppliedOnlyUnderTheSourceThatMirrorsIt`,
  `TestARisingDisableSuspendsEvenWhenTheAccountIsActiveAgain`, `TestALocalDisableStandsOverADocument`,
  `TestADocumentThatDoesNotKnowTheDisableNeverResumes`, `TestAnActiveDocumentResumesOnlyASuspension`,
  `TestADocumentThatResumesAndRevokesRunsInOneWrite`,
  `TestADemotionFromADocumentDropsPendingConsentsAndInvitesAndKeepsKeys`,
  `TestTheSameDocumentTwiceWithASubSecondTimeEndsNothingTheSecondTime`,
  `TestRisingSessionRevocationEndsOnlySessionsStartedBefore`,
  `TestRisingCredentialRevocationStopsKeysCreatedBeforeItAndItsMarginOnly`,
  `TestATeamIsRenamedOrClosedOnlyByAHigherWorkspaceVersion`,
  `TestATeamClosedByADocumentStopsEverythingAndKeepsItsData`,
  `TestATeamClosedOrReopenedAcrossAResetTakesItsStateFromTheNextDocument`,
  `TestAnOlderOrEqualDocumentChangesNothing`, `TestADocumentOfAnotherGenerationIsRefused`,
  `TestADocumentThatChangesAWorkspacesKindIsRefusedButItsRevocationsApply` and
  `TestAClosedDocumentEndsSessionsAndKeysAndSuspendsBeforeItsConfirmation`; generations, resets and
  holds, `TestAResetForgetsVersionsAndCursorAndKeepsTheHighestMarks`,
  `TestAResetWithTheSameGenerationLetsALowerVersionApply`, `TestAResetIsOneTransaction`,
  `TestACursorWrittenAgainstAnOlderResetIsRefused`, `TestACursorIsNeverWrittenForADirectoryWithoutARow`,
  `TestAResetCreatesAnAbsentDirectoryAtItsGeneration`,
  `TestAStrandingRemovalIsHeldUntilTheGenerationIsConfirmed`,
  `TestConfirmingTheGenerationEndsEveryHeldMembershipAndStrands` and `TestConfirmingTwiceChangesNothing`;
  sign-in, `TestANewPersonsDocumentIsAppliedInTheTransactionThatCreatesThem`,
  `TestANewPersonIsNotCreatedWithoutADocumentWhereTheSourceCreatesNoPersonalWorkspace`,
  `TestANewPersonWhoseDocumentSaysDisabledIsNotCreated` and
  `TestAReturningPersonsDocumentIsAppliedBeforeTheirSession`; people closed,
  `TestForgettingAClosedPersonDeletesThemAndTheirPersonalWorkspaceWithoutRefusals`,
  `TestForgettingTheLastReaderOfATeamMailboxStrandsItAndKeepsTheTeam`,
  `TestForgettingASuspendedPersonStrandsWhatWaitedForThem`,
  `TestForgettingAPersonWhoseMembershipIsHeldStrandsWhatTheHoldKept`,
  `TestAForgottenPersonsSubjectIsKeptUntilReleasedOrSwept`,
  `TestAPendingDeletionKeepsItsActorAfterThePersonIsForgotten`,
  `TestReleasingAPersonBlanksTheirActorAndLostUser` and `TestAForcedDeleteJournalsEveryMailboxItStrands`;
  `TestTheStartLineListsWhatWouldKeepAPersonalWorkspace`, `TestNoTransportCallsWhatOnlyAnExtensionMay`;
  and `TestThePlatformSourceCreatesNoPersonalWorkspaceAndRefusesEveryChange` (`internal/app`).
- Following an external directory, what it reads and asks: the pause (`internal/store`),
  `TestAPauseIsEmptyExactlyWhenTheMailboxMaySync` and `TestThePauseTableIsOneOrderForEveryCause`;
  the snapshot (`internal/workspace`), `TestASnapshotListsLiveAndRemovedMailboxesWithTheirState`,
  `TestASnapshotsPeopleFollowTheReaderRule`, `TestARemovedMailboxIsReportedAsLiveByDefaultInASnapshot`,
  `TestARemovedMailboxProtectsNobodyAndHoldsNoKeyInASnapshot` (the switch's two ways),
  `TestASuspendedHolderHoldsTheKeyButNeitherReadsNorWaitsNorIsProtected`,
  `TestASnapshotReadsIgnoreTheTeamsClosure`, `TestASnapshotNeverHoldsAnAddress`,
  `TestASnapshotRefusesMoreThanItsLimits` and `TestASnapshotIsOneConsistentRead`; the service,
  `TestAnAccessSnapshotIsOnlyOfAWorkspaceFromTheDirectorysSource`,
  `TestOneSwitchSaysWhatASnapshotReportsOfARemovedMailbox`,
  `TestTheJournalIsReadPerWorkspaceFromACursor`, `TestTheJournalNamesAKeysCreatorAsItsActor`,
  `TestTheJournalsReadersAreWhoReadTheMailboxThen` and
  `TestAnExtensionListsTheFollowedWorkspacesAndWhichChanged`; freshness,
  `TestEveryActThatWidensAccessAsksForFreshness` (the eleven acts),
  `TestFreshnessNamesTheRecipientOfAKeyOrOfRaisedFlags`,
  `TestFreshnessIsNeverAskedToReadRemoveRecoverOrLowerAccess`, `TestFreshnessIsNeverAskedForAKeysOwnActs`,
  `TestFreshnessIsAskedOutsideEveryWriteTransaction`, one
  `TestAFreshnessThatEndsTheActorsMembershipFailsTheActAt<Site>` per act (`SetAccess`, `SupplyKey`,
  `WriteFirstKey`, `WriteNewKey`, `SetKeyAccess`, `CreateWorkspaceKey`, `AddAccount`, `StartOAuth`,
  `SetMailboxSync`, `SetFlags`, `SendMessage`) and
  `TestARefusedFreshnessPassesItsOwnErrorAndMapsAnyOtherToUpstream`; the revocation
  (`internal/account`), `TestAGoogleMailboxDeletedForGoodHasItsGrantRevoked`,
  `TestOnlyAGoogleGrantNoOtherMailboxHoldsIsRevoked` and
  `TestAFailedRevocationNeverStopsTheDeletionNorLogsTheToken`, with
  `TestForgettingAPersonRevokesTheGoogleGrantsOfTheMailboxesThatGoWithThem` (`internal/service`);
  the daemon (`internal/app`), `TestAnExtensionsStateSurvivesARestartAndRollsBackWithTheDatabase`,
  `TestAnExtensionsStateIsTheDaemonsAndNamedWithinBounds` and
  `TestADaemonFollowingADirectoryNamesThePersonalWorkspacesADocumentWouldNotReplace`; the console
  (vitest), `a failure keeps the server’s Retry-After` and `a workspace that vanished under a live
  session falls back to the personal one`.

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
28. **Removing a mailbox deleted it at once, by an owner or an admin.** A removal made by mistake,
    or by an admin the team's owners did not agree with, lost the mailbox's index, key and grants
    for good, and its readers learnt of it by finding it gone. *Resolution (the owner,
    2026-10-13):* a removed mailbox is kept 30 days, used by nothing, recoverable by its
    workspace's owners, and then deleted for good; only owners remove a team's mailbox (an admin
    still abandons a link of their own that never connected, which holds nothing); its readers are
    told. It protects no last reader meanwhile, so a team mailbox nobody who could read it is left
    to read is not recovered. The operator deletes any removed mailbox for good, to erase one when
    its person asks, and recovers none of a person's or a team's. Its address stays taken until it
    is deleted for good. See [Removed mailboxes](#removed-mailboxes); the open sync text says so
    at `2026-10-open-sync-4`.
29. **What a directory says of a person, keyed by the person or by the subject.** The first design
    keyed `directory_people` by the directory's subject. But a person deleted here keeps their row
    until an extension releases them, which waits for what it still has to report of them, and the
    same subject may sign in again meanwhile, as a new person who needs a row of their own beside
    the gone one: keyed by subject, that sign-in would wait for the release or overwrite the row
    and lose the mapping the pending reports need. *Resolution (recorded for the owner,
    2026-10-14):* the row is keyed by issuer and person, and the subject is unique only among the
    rows of people still here (`directory_people_subject … WHERE gone_at = 0`); every lookup by
    subject reads the live row. See
    [Following an external directory](#following-an-external-directory).
30. **What an external directory decides, against the protections.** The protections keep a team
    an owner and a mailbox a reader, refuse changes to a closed team and to people who are not
    active, and a disable here stands over a suspension. A directory that owns workspaces and
    memberships decides who belongs, and a refusal here would leave this server serving someone the
    directory removed. *Resolution:* the directory exceptions. What it says applies in a closed
    team too, to any person, without the last-owner or the last-reader refusal; a mailbox its end of
    a membership or a person leaves with nobody who reads it is journaled stranded; a membership it
    ends while its generation is unconfirmed, and whose end would strand a mailbox, is held
    (disabled, reading nothing, its flags and grants kept) until a document lists it again or the
    generation is confirmed, so a directory restored from an older copy cannot strand a mailbox
    before it says its data is final; a document whose workspaces are refused still applies what it
    revoked; a personal workspace is replaced only while it holds no mailbox and no live key. What a
    disable here decides stays: a directory never resumes a person disabled here, nor one whose
    disable its document does not know of.
31. **A removed mailbox in what a directory is told.** Here a removed mailbox has no reader and
    protects nobody: its readers may leave while it is kept, and a recovery brings back only those
    who still qualify. A directory that protects people on every product's word asks one rule for
    all of them, and another product keeps protecting a deleted item until its purge. *Resolution
    (pending the owner, 2026-10-14):* the snapshot reports a removed mailbox's people as they would
    be once it is recovered — who would read it and hold its key at its epoch, its kept last readers
    protected — until it is deleted for good, behind one switch (`service.RemovedInSnapshot`) whose
    other way reports nobody; both are tested. This server's own refusals are unchanged either way.
32. **A provider's grant outliving its mailbox.** A mailbox deleted for good takes its stored
    token, but the grant behind it stays at the provider until its person removes it there.
    *Resolution:* Google's grant is revoked at its revocation endpoint once the deletion commits,
    best effort, never while another mailbox here holds the same grant (Google keeps one per
    address and OAuth client); Microsoft has no per-grant revocation (only every session of the
    person, which is not this server's to end), and a password has nothing to revoke: both lapse.
33. **Freshness, and who decides.** A directory elsewhere may have ended someone's membership a
    moment before they act here, and an act that shares a key or widens access cannot be taken back.
    *Resolution:* the freshness hook, asked before eleven acts of a person signed in and outside
    every write, lets the extension read and apply the directory's answer first; it never
    authorizes, and the act's write asks again who may, so an answer that ended the actor's place
    stops it there. An outage is `internal` with a `Retry-After`, which the console keeps
    (`Failure.retryAfter`), and a page whose workspace vanished under its session reads its
    workspaces again once and shows the person's own.

## Not in this phase

- A send tool over MCP (the planned draft and confirmed send); keys send over REST.
- Deleting a team workspace but with its last member; moving a mailbox between workspaces (link it
  again instead).
- Closing a team from the console, or by its owners: the operator closes a local team, and a
  team from another source closes where it is managed.
- An edition's own screens for teams from a directory (the open edition's Members section is for
  the local source only); what an extension that follows a directory does with the seams above is
  that edition's.
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
| 0020 (removed mailboxes) | `internal/store/migrations/0020_removed_mailboxes.sql` and `migrate_twenty_test.go`; `store/readers.go` (`LiveSQL` in the one rule), `eligibility.go` (a removed or held mailbox syncs nothing), `consent.go` (`SetMailboxSync` lifts the hold; a sync turned off, and `StopBoundTx`, tell the readers), `mailboxevents.go` (the journal and the notices); `account/removal.go` (removing, recovering, deleting for good, the sweep), `store.go` (`Visibility` and `Get` leave a removed mailbox out; `GetRemoved`, `ListRemoved`) and `registry.go` (an address kept removed is a conflict); `workspace/grants.go`, `keys.go`, `sealed.go`, `workspace.go` (nothing finds a removed mailbox) and `people.go` (who removed and who acted forgotten with their person); `auth/keys.go` (no new key restricted to a removed mailbox); `service/removed.go`, `notices.go`, `accounts.go` (only owners remove), `consent.go`, `sync.go` (`held`) and `events.go` (`NewNotices`); `app/sweep.go`; `api/removed.go`, `api/events.go` (`event: notice`) and the fixtures; the command line (`account removed`, `recover`, `purge`); the open sync text `-4`; the console's removed mailboxes, notices and hold ([`console.md`](console.md)) |
| 0021 to 0023 (following an external directory, storage) | `internal/store/migrations/0021_stranded_mailboxes.sql` (a rebuild), `0022_directories.sql`, `0023_workspace_changes.sql` and their tests; `store/mailboxevents.go` (strandings, causes, the lost person, a removal's end, `MailboxLabel`), `consent.go` (`bound_stopped`), `readers.go` (`HeldReaderSQL`), `directory.go`, `extstate.go`, `changes.go`; `workspace/strand.go` (`Stranding`, `JournalStrandedTx`), `people.go` (a followed person's name kept in the journal until released); `auth/closure.go` (a deleted person's directory rows marked gone); `account/removal.go` and `store.go` (a link that never connected, deleted in a workspace from another source, journaled) |
| Following an external directory, its documents | `internal/workspace/mirror.go` (`ValidExternalID`, `MirrorWorkspaceTx`, `ReplacePersonalTx`, `MirrorMemberTx`, `EndMembershipTx` and its holds, `HeldMembershipsTx`, `PersonalReplacementBlocks`), `reader.go` (`StrandCandidatesTx`), `members.go` (`Member.Held`), `source.go` (`Platform` made real); `auth` (`ExternalSignIn.PersonCreatedTx`, `IdentityUser`, `ResumeTx`, `RevokeSessionsStartedBeforeTx`, `Keys.RevokeCreatedBeforeTx`; a disable, `DisableBy` naming its actor, and a forced reset journal what they strand); `store/directory.go` (the listings and the sweep); `service/directory.go` (the documents, the directory's calls, a closed person forgotten and released), `external.go` (the document at sign-in), `closure.go` (`removePerson`: a deletion journals what it strands); `app/sweep.go` (`SweepDirectory`) |
| Following an external directory, what it reads | `internal/store` (`ReadTx`, `PauseSQL` beside the sync rule, `KeptWaitingSQL`, `journalreads.go`: the journal per workspace with its readers and a key's creator, its head, the followed workspaces); `workspace/snapshot.go` (`SnapshotTx`); `service/snapshot.go` (`AccessSnapshot`, `RemovedInSnapshot`), `journal.go` (`MailboxJournal`, `MailboxJournalHead`, `FollowedWorkspaces`, `WorkspaceChanges`, the extension state), `freshness.go` (`Freshness` and its eleven call sites in `workspaces.go`, `mailboxkeys.go`, `workspacekeys.go`, `accounts.go`, `consent.go`, `actions.go`, `send.go`); `app` (`Options.Freshness`, `Options.RemovedInSnapshot`, `Deps.State`, the start line of personal workspaces a document would not replace); `account/revoke.go` (a Google grant revoked once its mailbox is deleted for good) and `registry.go`, `removal.go`, `store.go` (the token opened in the deleting transaction); `web/src/state/failure.ts` (`Failure.retryAfter`), `workspaces.ts` (`workspaceVanished`), `accounts.ts` and `storage.ts` |
| 0011 (2026-10-06, step 1) | `internal/store/migrations/0011_team_mailboxes.sql` and `migrate_eleven_test.go`; `store/eligibility.go` (a team mailbox's consent and a reader), `store/consent.go` (`SetMailboxSync`, `StopBoundTx`, a person's withdrawal personal only); `workspace/reader.go` (the last reader, `HasReaderTx`), `grants.go` (effective manage, the directory), `members.go` (invites expire, no linker), `people.go` (blocks: last owner, last reader of a team that outlives them, a bound consent someone else reads; attribution blanked); `account/store.go` (the person of a personal mailbox, `linked_by`, the team's consent at the link, `Visibility` by role) and `registry.go` (`LinkerID`, `RemoveChecked`, a sole member's teams removed with them); `auth/closure.go` (a disable changes nothing for someone already disabled); the service's grant rules, `RemoveAccount` with `confirm`, `SetMailboxSync` (never on for a mailbox nobody reads), closure (`team_syncs_stopped`); the API's routes and fixtures; MCP's `list_accounts` text (read access marked); the command line; the open texts `-3` and `-2` |
