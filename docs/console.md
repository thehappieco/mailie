# The server console and its API

The console is where a person signs in to **their** account on a Mailie server and connects their
own mailboxes: Gmail, Microsoft 365/Outlook, iCloud Mail and generic IMAP, in their personal
workspace or in a team. The open console (`web/`) has five sections: **Mailboxes**, **Members**
(the people of the team shown, or making a team), **API keys & MCP**, **Storage** and **Account**
(name, password, sessions, and what they allow the server to do: sync and actions). No mail is read
or written there; a tool does that, with a key.

This document is the console's architecture and the REST API it uses, which is the same API the
command line and any other client use. The MCP server is in [`mcp.md`](mcp.md), backups in
[`backup.md`](backup.md), and how to register the OAuth clients in the
[README](../README.md#mailboxes-and-providers).

## How it is served

```
browser ──► <MAIL_PUBLIC_URL> ──► mailserver
            /              → internal/webui (files of MAIL_WEB_DIR, a single-page app)
            /v1/...        → internal/api   (REST)
            /mcp           → internal/mcp   (unless MAIL_MCP_HTTP=false)
            /oauth/return  → the single-page app (a console route, not a server one)
```

- **One origin.** The console and the API are served by the same process on the same origin: no
  CORS, no cookie, and the CSP can say `connect-src 'self'`. In development Vite
  (`localhost:5174`) proxies `/v1` and `/mcp` to the daemon (`127.0.0.1:8080`) for the same effect.
- **`internal/webui`** serves a build from `MAIL_WEB_DIR`, never through `go:embed`, so `go build`
  and `make check` never need Node. Empty (the default) is an API-only daemon. It is mounted last,
  as a catch-all: an unknown path under `/v1/`, `/mcp` or `/.well-known/` answers a **JSON 404**,
  never `index.html`; other paths without an extension get `index.html` (which covers
  `/oauth/return`). Every response carries a strict CSP, `X-Frame-Options: DENY`,
  `Referrer-Policy: no-referrer`, COOP/CORP `same-origin` and `nosniff`. The directory is opened
  with `os.Root`: neither `..` nor a symlink escapes it.
- **Other origins.** `MAIL_CONNECT_SRC`, empty by default, adds origins to the header's
  `connect-src`, for an edition whose pages talk to a service on another origin: a list separated by
  spaces, each a scheme and a host with an optional port, without a path, query, fragment or
  wildcard; the host a name or an IPv4 address, since a CSP source cannot hold an IPv6 literal (a
  browser drops `http://[::1]:8081` as invalid; use `localhost`); `http` only for loopback
  addresses and names under `.localhost`, and never in `prod`. `config.Load` refuses anything
  else. Unset, the header is the policy it always was, byte for byte. A browser enforces every
  policy a page carries, so an origin must also be in the meta policy of that edition's
  `index.html`; the open console's says `connect-src 'self'`, so with it the setting changes
  nothing.
- **The console never shows the server's `message`.** It translates the `code` (and the operation
  it was doing) into its own words: English, and the Portuguese, Spanish, French and German
  catalogs in `web/src/ui/locales`.

### Editions

`web/src` is a **core** that every edition shares, plus the open edition in `web/src/open`. An
edition calls `configureEdition()` (`web/src/edition.ts`) before mounting: the console component
built on `components/ConsoleShell.vue`, the texts a person agrees to (sync, actions, a new key's
terms) with their revisions, what the frame calls itself (`shell`: the breadcrumb's root and the
sidebar's name; the open edition's is the server's "Console"), its sections as data (each one the
workspace shown's, or with `scope: 'person'` the person's own, where the breadcrumb names no
workspace), a few sentences it words its own
way, whether to show how to connect over MCP, whether people make and run teams on this server
(`teams`: the open edition's, for the local workspace source), whether the account section names
the person's role on this server (`serverRole`, the open edition's), and optional extras (a
byline, legal components, more translations). The core's own sentences never name the frame: they
say "this page". Other extension points are `onLiveEvent()`/`onLiveLagged()`/`onLiveAccess()`
(`state/live.ts`), `addDescriber()` for an edition's own operations (`ui/errors.ts`,
`state/failure.ts`), `onActionsConsent()` and the account section's pieces (`AccountPanel`,
`SyncPermission`, `ActionsPermission`, `PermissionRow`).

An edition whose people sign in another way (see
[Signing in through an extension](#signing-in-through-an-extension)) sets `signIn`, a component
that takes the password card's place on the signed-out screen inside the same frame (the brand and
appearance menu above, the notes about an ended session or a mailbox waiting to finish connecting,
`legal.signInFooter` below); no invitation signs anyone up there. Its component calls
`adoptSession(reply)` (`state/session.ts`) with what its route answered, the shape
`POST /v1/auth/login` answers: the same path the password sign-in takes to store the session and
load the person, so no edition keeps a session of its own. `signedOut` is called once the person has
signed out on purpose, after the session ended in this browser and the server was told, never when
one expires, is refused or ends in another tab; an edition may navigate away there. A person whose
`has_password` is `false` is not offered to change a password. The open edition sets none of these.

The hosted service's app is such an edition, kept in a private repository: it compiles `web/src`
from source and adds reading and writing mail. The direction is one way. Nothing under `web/src`
imports from outside it or through an alias, and the open console names nothing of the hosted
service (`web/test/editions.spec.ts` reads every file under `web/src` and `web/public`, and
`web/index.html`, for the names in `web/test/hosted.ts`; the build fails on an output file that
has one). Keep both in mind when you change the core: an edition you cannot see builds on it.

## People and credentials

### Sessions

A session is an opaque **bearer token, never a cookie**: 32 random bytes in base64url (43
characters, without a dot, which is how the daemon tells it from an API key `<prefix>.<secret>`),
stored as SHA-256, with an absolute lifetime of 14 days and no sliding renewal (one started through
an extension may be given less, never more: see
[Signing in through an extension](#signing-in-through-an-extension)). The browser keeps
it in memory and an encrypted copy in IndexedDB under a non-extractable AES-GCM key, tells other
tabs over a `BroadcastChannel` when it signs out, and drops it on any `401`. Routes for a person
(`/v1/auth/*`) refuse API keys.

### Passwords and sign-in

Passwords are hashed with Argon2id on the server (64 MiB, t=3, p=1), at least 10 characters of
valid UTF-8, with at most two hashes running at once. An unknown address, a disabled person and a
person with no password (one who signs in only through an extension) cost the same work as a wrong
password and get the same answer: a password is checked against a dummy hash in all three, and
nothing matches the empty hash a person without a password has. Changing the password ends every
other session; a person without one has no current password to prove, and the console does not
offer the change (`user.has_password` is `false` in `GET /v1/auth/me`).

A forgotten password is reset by the operator, with the daemon stopped:
`mailserver user password --bootstrap --email X`. It asks for the new password twice without
echo, or reads one line from standard input when that is not a terminal (never a flag or a
variable), applies the same rules as sign-up (bytes in another encoding, such as a Latin-1 file,
are refused: no sign-in could send them), and ends every session the person has in the same
transaction. A disabled person stays disabled. There is no route for it, by design: nothing remote
sets someone's password. `--email -` reads the address from standard input; the password must then
be typed at a terminal. It is also how a person who signs in through an extension, and has no
password, is given one.

### Invitations

Nobody signs up uninvited. An invitation is single-use, valid only for its address, for 7 days, and
records the role asked for.

- `mailserver user invite --email X [--role owner|member]` calls `POST /v1/users/invites` with
  `MAIL_ADMIN_KEY` (an unrestricted instance admin key; a signed-in owner may call the route too)
  and prints the link, `<MAIL_PUBLIC_URL>/#invite=…&email=…`. The code travels in the fragment,
  which the browser never sends to a server, and the page removes it as soon as it reads it.
- `mailserver user invite --email X --workspace ID [--role owner|admin|member]` is a **team
  invite** (`POST /v1/workspaces/{id}/invites`): a person who already has an account accepts it
  signed in (`POST /v1/auth/invites/accept`). A new address signs up with it, and joins the team,
  only when the operator or an instance owner (still active) made it: any person may create a team
  and invite into it, and that must not be a way to create an account for someone else's address.
  Anyone else's team invite offered at sign-up is `403` ("sign in with that address and accept
  it"), whether or not the address has an account, and is not spent. Joining a team gives access to
  no mailbox. See [Workspaces](#workspaces).
- `--bootstrap` writes to the database directly, for the first person, while no daemon runs.
  Without `MAIL_PUBLIC_URL` its link points at `http://localhost:5174` and the command says so.
  Without `MAIL_PUBLIC_URL` the route answers `409`: a link is never built from a request's `Host`.
- The invitation decides the role, when it is made; signing up first or last changes nothing. The
  first owner comes from `user invite --bootstrap --role owner`. Without `--role`, `--bootstrap`
  makes an `owner` invitation while the server has no active owner and no owner invitation waiting,
  and a `member` one otherwise; the route's default is `member`.
- Unused invitations are deleted within 30 days of expiring; a used one stays with the person and
  goes when they are deleted.

Closing someone's account is the operator's, or an instance owner's signed in (never on
themselves): `mailserver user disable --email X` ends their sessions and revokes their keys;
`mailserver user delete --email X` deletes the person, the mailboxes they linked with their
credentials and index, their sessions, keys, invitation and personal workspace, and every team
whose only member they were. Both take `--email -` to read the address from standard input, which
keeps it out of `sudo` logs and shell history, and refuse without `--force` to remove the last
active owner of the server, the last active owner of a team others remain in, or the person a team
mailbox others read syncs under; the refusal names those teams and mailboxes.

### Signing in through an extension

A binary that embeds the daemon (`internal/app`) may sign people in through an identity provider it
trusts, on routes of its own (`Options.Extensions`). The core has no provider and names none: the
extension does the provider's protocol, then calls `Service.SignInExternal` with who the provider
says the person is, and answers its page with the `Session` that comes back, exactly as
`POST /v1/auth/login` answers the console; the console adopts it the same way
(`adoptSession`, below). No route of the core calls it.

- **Identities.** The provider names a person by its **issuer**, an origin exactly as a browser
  writes one (`https://host[:port]`, or `http://` on loopback or a name under `.localhost`, with no
  path), and a **subject**, its own stable id for them (1 to 255 bytes of text, never an address).
  The pair is linked to the person its first sign-in created (`user_identities`) and signs that
  person in from then on, whatever the provider later says of the address.
- **A pair seen for the first time only ever creates a person.** It is believed only with an
  address the provider verified (`email_verified`); otherwise nothing is created
  (`not_authorized`), and the answer is the same whether somebody here has the address or not. Nor
  with an address that lower case would turn into another (`bad_request`): the Kelvin sign is not
  the letter K, nor the dotted capital I the letter i, and the mailbox the provider verified is not
  the one lower case makes of it, which may be somebody else's here. With nobody at that address,
  a new person is created: an instance **member**, never an owner, with the name given, no
  password, and what the workspace source creates for a person, exactly as signing up creates
  one; the instance invitations waiting for the address are spent, as signing up spends them. The
  name is the provider's, so it is made into one the server takes (control characters become
  spaces, a name past 120 characters is cut) rather than refused; the person a linked pair signs in
  keeps theirs, whatever the provider now calls them. All of this, and the session, is one
  transaction.
- **An identity never takes over a person who exists.** Accounts are never linked by matching
  addresses: when somebody here already has the address, compared without regard to case, the
  sign-in is refused (`conflict`) and nothing is created or linked, whoever they are, however they
  sign in (with a password, or through another identity from this provider or another), and
  whether or not they are disabled. Their password, sessions, identities and workspaces stay as
  they were. A server that turns an extension's sign-in on over people who signed up with a
  password therefore either leaves them their passwords (`ExternalSignInOnly` off) or closes their
  accounts first (`user disable`, then `user delete`, which removes the mailboxes they linked and
  everything indexed from them), after which they sign in through the provider as new people and
  link their mailboxes again. A disabled person whose own pair signs in is refused
  (`unauthorized`), as their password sign-in is.
- **No password.** Such a person's `password_hash` is empty and `password_changed_at` is 0. No
  password check accepts an empty hash (it costs a dummy derivation, as an unknown address does),
  and `user.has_password` is `false`. `user password --bootstrap` gives them one.
- **The session's lifetime** is the extension's to choose: more than nothing and at most 14 days
  (`auth.SessionTTL`), absolute from its start. Nothing renews a session on use, and no statement
  can move an existing session's expiry later: the schema refuses an update that would
  (`sessions_expiry_fixed`, an upsert's included) and an insert under an id or token a session
  already has (`sessions_started_once`), which is how `INSERT OR REPLACE` would get around the
  first. The code only ever starts a session new, under a new id and token.
- **A personal key outlives the session it was created in.** A key the person creates while
  signed in (`POST /v1/me/apikeys`) lasts what they chose, 30, 90 or 365 days, whatever their
  session's lifetime: a short session bounds how long a browser stays signed in, not what the
  person deliberately hands a tool, which is what a key is for. So a session kept short because the
  provider cannot yet tell this server that it closed or locked someone does not close that gap for
  keys: until it can, the operator disabling the person here (`user disable`) is what revokes their
  keys and ends their sessions, in one transaction.
- **Key pins.** `Service.PinIdentityKey(issuer, subject, keyID, key)` keeps the public keys an
  identity is known by (`identity_key_pins`, at most 4096 bytes each): inserted the first time a key
  id is seen, then read back, in one transaction, so of two sign-ins racing each other both get the
  key that won. A pin is **never replaced**: an update is refused, and an insert under a key id
  already pinned changes nothing, whatever its conflict clause. It is **deleted only with the person
  it identifies**: the schema refuses to delete a pin while its identity is linked, and deleting the
  person (`user delete`, the second step of closing an account) removes their identities and then
  their pins in the same transaction. Disabling the person, the first step, keeps both, as it keeps
  their password: a person switched back on signs in as before, and the pin must still hold then.
  An extension pins a key before the sign-in that links its identity; when that sign-in is
  refused (an address not verified, an address somebody here has already), the pin signs nobody
  in, and the hourly retention sweep deletes it once it is ten minutes old
  (`auth.UnlinkedPinGrace`), so it is kept a little over an hour at most.
- **`ExternalSignInOnly`** (`app.Options`, set only by a binary that embeds the daemon; there is no
  variable, and `serve` never sets it) is a server whose people sign in only that way. The service
  then refuses, `not_authorized`, every route that signs in with a password, signs up or accepts an
  invitation, changes a password, or creates an invitation to the instance or into a team:
  `POST /v1/auth/login`, `/v1/auth/signup`, `/v1/auth/invites/accept`, `/v1/auth/password`,
  `/v1/users/invites` and `/v1/workspaces/{id}/invites`. The command line's `--bootstrap` commands
  still write to the database, but an invitation they print signs nobody up there. Left at its zero
  value nothing changes.

### Keys and scopes

A key is `<prefix>.<secret>`: eight hex characters, then 32 random bytes in base64url. Only its
Argon2id hash is stored; the prefix names it in lists and logs. Scopes are ordered,
`read < write < send < admin`. There are two kinds:

- **Instance keys**, the operator's: any scope, optionally restricted to some of the operator
  workspace's mailboxes (`--accounts`), at most 365 days. Over REST and MCP alike they reach the
  operator workspace's mailboxes and nothing else: never a person's. They are managed through the `/v1/apikeys` routes,
  which the daemon mounts only with `MAIL_ADMIN_API=true` (off by default; otherwise they answer
  404). `mailserver apikey create --scope SCOPE --name NAME`, `apikey list` and
  `apikey revoke PREFIX` are clients of those routes, so they need it too. Without it the only way
  to issue one is `mailserver apikey create --bootstrap`, which writes to the database with the
  daemon stopped.
- **Personal keys**, which a person creates in the console for their own tools (see
  [API keys](#api-keys) below). They act as that person, and they are the only way a tool reaches
  a person's mailbox.

A session counts as a person with every scope; what it may do to a mailbox still depends on
their grant on it and their consent.

### Rate limits

Sign-in and sign-up: 60 a minute per address (burst 20) and 5 a minute per email address, before
any hashing; a password change, 5 a minute per person. Authenticated requests spend from a bucket
per address (600 a minute, burst 60). A wrong key secret spends from a tight bucket for that key's
prefix (30 a minute, burst 10), and a key whose prefix does not exist from a tight bucket for the
address, consulted only once the prefix is not found. No failure is charged to the address as a
whole: behind a NAT, or a proxy missing from `MAIL_TRUSTED_PROXIES`, one address is a whole office.
IPv6 addresses count per /64. A throttled request gets `429` with `Retry-After`.

## Workspaces

Every mailbox belongs to a **workspace** (`docs/workspaces.md` is the long form): a person's
**personal** workspace, made with them; a **team**, whose members are `owner`, `admin` or `member`;
or the one **operator** workspace, which has no members and holds the mailboxes the command line
adds with an instance key.

- **Every mailbox belongs to its workspace.** A personal mailbox is its person's; a team's mailbox
  is the team's, whoever linked it (`linked_by` in the access directory is attribution only).
- **Using a mailbox** takes active membership in its workspace and a **grant** on it: `read` (its
  index: folders, messages, events, storage), `act` (needs `read`) and `send`. **Owners and admins
  manage every mailbox of their workspace by their role**: they see its card (address, state, sync
  counters, never what it holds), re-authorize it, remove it, and decide who holds what. A member
  sees only the mailboxes they hold a grant on; a stored `manage` gives a member the card and
  re-authorizing. Whoever links a mailbox gets `read`, `act` and `send` on it.
- **No role reads.** `read` passes only from an owner or an admin who reads the mailbox now; `act`
  (to someone who reads) and `send` from any owner or admin, to anyone in the team, themselves
  included. The operator grants `manage` to members and revokes.
- Another workspace's mailbox, and one the caller neither holds a grant on nor manages by a role,
  answer `not_found`, never `forbidden`; a mailbox the caller sees without the flag an operation
  needs answers `not_authorized`. The rule lives in `internal/service` and runs in SQL on every
  call, so listing filters with the same rule as fetching one, and a person's key never reaches a
  mailbox its person lost.
- **Only owners and admins** (and the operator) list a team's members and its access directory,
  invite, and remove members; a member is `403`. A member or an admin does not leave by
  themselves; an owner leaves while another owner remains.
- `users.role` is the **instance** role of a self-hosted server: an `owner` invites people to the
  server and disables or deletes them, and sees no mailbox by being one. The `member` role is
  everybody else.
- An address is linked at most once per workspace; the same address linked in two workspaces is
  two independent mailboxes. Removing a mailbox repeats its id (`DELETE /v1/accounts/{id}?confirm=<id>`).

Protections, each `409` and marked in advance in the listings (`last_owner` and `last_reader_of`
on members, `readers` and `no_reader` on the directory): a team keeps an active owner, and a team
mailbox someone reads keeps a **reader** — the last person who can read it keeps `read`, and is not
disabled, removed, or closed without `force`. A team mailbox nobody can read (a forced closure)
syncs nothing, is never switched on again (`409`), and is marked `no_reader`; its owners and admins
remove it, or remove it and link it again, or turn its sync off to delete what is still indexed. A team mailbox syncs under its **workspace's consent** ([Sync and consent](#sync-and-consent)).

### Workspaces in the console

The console decides nothing here: every rule is the server's, and a screen only offers what the
caller's role and flags allow and says beforehand what a protection refuses (`web/src/ui/access.ts`,
whose rules are tests of their own). A refusal is said in the console's words, from its `code`.

- **The workspace shown.** The sidebar (the drawer on a phone) has a switcher once the person
  belongs to more than one workspace, and the header's breadcrumb names the one shown on the
  sections that are a workspace's (Mailboxes, Members, Storage), never on the person's own (API
  keys & MCP, Account), which an edition marks `scope: 'person'` (its account section always is).
  It is kept in memory for the tab; the last one chosen is remembered for each person apart, in
  this host's local storage under a key naming only their opaque user id
  (`mailie_workspace:usr_…`, never a cookie, so no request and no other host carries it; someone
  else signing in on the same browser does not open on it, and signing out, even everywhere,
  clears no preference, this one included), and checked against `GET /v1/workspaces`, which is
  read before any list: one the person is no longer in is never asked for, and the personal
  workspace is shown instead. The mailboxes (`GET /v1/accounts`), Storage (`GET /v1/me/storage`)
  and the event stream (`GET /v1/events`) pass `?workspace=`; a card
  of another workspace that an answer names is left out of the list. Choosing another workspace
  reads its lists again and opens its stream from now; an answer for the one before that lands
  later is dropped. A server without workspaces (`404`) lists everything together. Any other
  failure to read them holds every list and the stream, and says why, rather than read them
  without `?workspace=`, which would show a team's mailboxes as the person's own: while the console
  is shown it asks again by itself (2 s doubling to 30 s, or as long as a rate limit asks), and
  reads the lists once the workspace is known. Refresh reads the workspaces again too, as does a
  refusal for want of a role (`not_authorized` from a team or access route): a role changed
  elsewhere, or a team joined in another tab, shows without reloading.
- **The person's own settings are not a workspace's.** API keys act as their person in every
  workspace (until step 3 makes keys a workspace's, [`workspaces.md`](workspaces.md)), so the keys
  section names a key's mailboxes, and the new-key dialog offers them, from every workspace
  (`GET /v1/accounts` without `?workspace=`, grouped by workspace); only mailboxes the person may
  read are offered. The dialog that turns the person's own sync off says it deletes the index of
  their personal mailboxes, and, to someone in a team, that a team's mailboxes sync under the
  team's consent and keep syncing, but for one they linked before the upgrade whose carried-over
  consent no owner or admin has confirmed yet, which stops with its index. It reads no list first.
- **Each mailbox** says on its card what the person may do with it (`access`: Read, Act, Send,
  Manage). Folders and Sync need `read`; authorizing again needs manage; a mailbox seen without
  `read` (an owner's or an admin's card of a team mailbox, say) says so instead of offering them,
  and one that needs authorizing tells whoever does not manage it that an owner or an admin has
  to. Removing is a team's owners' and admins' (or a personal mailbox's person's): it asks for the
  mailbox's address typed and sends its id as `confirm`.
- **A member of a team** gets the switcher and cards for the mailboxes they hold a grant on
  (re-authorizing where they hold `manage`; an edition with Mail and Compose offers those they read
  or may send from). No Members, no Access, no team sync switch and no removing: one line instead,
  on the team's mailboxes and in Members, says that its owners and admins manage the team's people
  and who can use each mailbox; the console never asks for the members or the directory, which the
  server refuses them. No Leave. Nor is the person's own consent card asked in a team: a team's
  mailboxes sync under the team's consent, and a member's sheet says that its owners and admins
  turn it on.
- **Who can use a mailbox** (the sheet's Access, in a team, for its owners and admins): every
  active member with their flags, ticked and then saved. `read` is offered only to a viewer who
  reads the mailbox; `act` only for someone who reads; `send` for anyone; `manage` for members
  (owners and admins manage by their role, so theirs is shown ticked and is not theirs to change).
  The mailbox's last reader (`last_reader_of`, or `readers` of 1) is marked, and taking their read
  is not offered; an owner or an admin who does not read it is told that they cannot give Read,
  not even to themselves; a mailbox nobody can read
  (`no_reader`) says so, and that removing it, or removing it and linking it again, is the way out.
  A change that only takes flags away is `DELETE …/access/{user}?flags=` naming them; one that gives
  anything sets the grant exactly (`PUT`, all four). The sheet also shows the mailbox's sync for
  the team — who turned it on, when and to which revision, and whether it is the current one or a
  consent the upgrade carried over from its linker (still bound to them), or one kept stopped with
  its index because they were disabled — with the switch: on shows the sync text and sends its
  revision (the console's own, never the server's: a server that already asks about another text is
  offered a reload); off asks for the address typed and says the index is deleted for its
  `readers`. A consent to an earlier text, or a carried-over one, offers confirming it for the team
  in place of the switch. One kept stopped says that its index stays only while it is stopped:
  turning it on resumes from it, and turning it off, removing the mailbox or deleting the account
  of the person it is still bound to deletes it. A mailbox nobody can read offers no way to turn
  it on, and offers turning off alone while something of it is still on or kept. An owner or an admin sees every team mailbox's card in the list, from the role; the
  access directory (`GET /v1/workspaces/{id}/access`) is read again whenever one of the team's
  mailboxes leaves or joins their list.
- **Connecting a mailbox** is offered only where the person may link one (their personal
  workspace, or a team they own or administer): in a team where they may not, the team's list says
  that its owners and admins connect its mailboxes and offers to show the personal workspace. It
  asks where once the person owns or administers a team: their personal workspace, listed first,
  or one of those teams (`workspace_id`). Into a team it shows the sync text with "Turn on sync for
  {team}", which sends `sync_consent_version`; left unticked, the mailbox is linked with sync off.
  The console shows the workspace chosen before connecting, so the new card lands in the list it
  belongs to; a provider's return shows the workspace of the mailbox it authorized.
- **Events.** `event: access` reads the list again (once for several, a second later), Storage if
  it was read, and what was read of the team shown (its members and its directory), and forgets the
  folders of a mailbox no longer readable; an edition hears it through `onLiveAccess()`. A stream narrowed to a workspace the person is no longer in ends with
  `not_found`: the console reads their workspaces again, shows another and says which went.
- **Members** (the open edition, `teams`): shown for the personal workspace, where a person
  creates a team (and becomes its owner) and sees the teams they are in, and, to its owners and
  admins, for a team made here (`source: local`); a team mirrored from elsewhere is changed there,
  and the section is not shown for it. A team's people with their roles: an owner changes anyone's
  role and status, removes anyone, and leaves while another owner remains; an admin disables,
  enables and removes members only. The last active owner and the last reader of a mailbox
  (`last_reader_of`) are marked, and the actions their protection refuses are not offered. The
  server works those marks out across the team, so the members are read again whole after any
  change of role, status or membership, and whenever one of the team's mailboxes leaves or joins
  the person's list, and so are the pending invitations when they were read, since any such change
  ends the ones its person made; the person's own role follows their row as listed. Disabling and removing ask
  first, saying what goes (the person's grants in the team, its invitations still waiting for
  them, the invitations they made there). Owners and admins rename the team and invite (an admin,
  members only): the link is shown once, in a dialog that only its own buttons close, copied on
  request and never kept, in storage or in the list of pending invitations, which only revokes.
  Before an invitation is made the dialog says who its link works for: someone with an account
  here, signed in; and someone without one only when the person inviting is an owner of the
  server.
- **An invitation opened signed in.** Where teams are made here, an invitation link no longer signs
  the browser out: the remembered session is restored and asked to join the team the invitation
  names (`POST /v1/auth/invites/accept`), saying that joining gives access to no mailbox. One for
  another address offers signing out to use it. Signed out, the link still opens the sign-up form,
  and someone with an account signs in instead and then accepts it. The invitation's code stays in
  memory until it is used or set aside. Where teams are not made here (an edition without `teams`),
  an invitation only ever creates an account, and one the sign-up refuses says only that, and to
  ask for a new one: there is no signing in to accept it.

## Errors

Every error is `{"code": "…", "message": "…"}` with one of seven codes:

| Code | Status | |
|---|---|---|
| `unauthorized` | 401 | no credential, or one that does not work; never which check failed |
| `not_authorized` | 403 | a working credential that may not do this |
| `bad_request` | 400 | invalid input; unknown JSON fields and unknown or repeated query parameters included |
| `not_found` | 404 | does not exist, or belongs to someone else |
| `conflict` | 409 | the state does not allow it now (consent off, the mailbox needs authorizing, a stale folder, …) |
| `rate_limited` | 429 | with `Retry-After` |
| `internal` | 500 | the server failed; `message` may say `upstream: …` when the mail server could not be reached |

`message` is for people reading logs; clients branch on `code`.

## Routes

The exact JSON of the routes the console uses is in `internal/api/testdata/contract/`, written by
the real handlers (`go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`) and
checked by `web/test/contract.spec.ts`: when one side changes, a test breaks.

| Route | Who | What |
|---|---|---|
| `GET /v1/healthz` | anyone | `{status, version, uptime_seconds}` |
| `POST /v1/auth/login` | anyone | `{email, password}` → `Session` |
| `POST /v1/auth/signup` | anyone | `{invite, email, name, password}` → `Session` (201); an instance invite, or a team invite the operator or an instance owner made |
| `GET /v1/auth/me` | session | `{user, session}`; `user.has_password` is `false` for a person who signs in only through an extension |
| `POST /v1/auth/logout` | session | `{everywhere?}` → 204 |
| `POST /v1/auth/password` | session | `{current, next}` → a new `Session`; every other session ends |
| `PUT /v1/auth/profile` | session | `{name}` → `User` |
| `POST /v1/users/invites` | instance owner signed in, or unrestricted instance admin key | `{email, role?}` → `Invite` |
| `POST /v1/auth/invites/accept` | session | `{invite}` → `Workspace`: joins the team with the invite's role |
| `POST /v1/users/disable` | owner signed in (never on themselves), or unrestricted instance admin key | `{email, force?}`: sessions ended, keys revoked, and `team_syncs_stopped` |
| `POST /v1/users/delete` | owner signed in (never on themselves), or unrestricted instance admin key | `{email, force?}` → what was deleted, and `team_syncs_stopped` |
| `GET /v1/providers` | read | which providers and flows **this** caller can use, the default first |
| `GET /v1/workspaces` | read | the caller's workspaces with their role in each; an instance key the operator workspace; the operator every workspace, with counts |
| `POST /v1/workspaces` | session; operator | `{name}` (the operator adds `owner_email`) → `Workspace` (201), a team |
| `PATCH /v1/workspaces/{id}` | team owner or admin; operator | `{name}` → `Workspace` |
| `GET /v1/workspaces/{id}/members` | owner or admin; operator (a member: `403`) | `[Member]`, with `last_owner` and `last_reader_of` |
| `PATCH /v1/workspaces/{id}/members/{user}` | owner: anyone's; admin: members', never to admin or owner; operator | `{role?, status?}` → `Member`; any change expires the invites the person made there |
| `DELETE /v1/workspaces/{id}/members/{user}` | owner: anyone, themselves while another owner remains; admin: members; operator | 204; their grants there go, their invites there expire |
| `GET/POST /v1/workspaces/{id}/invites`, `DELETE …/invites/{invite}` | owner; admin (member invites); operator | team invites: `{email, role?}` → `TeamInvite` with its `url` (201) |
| `GET /v1/workspaces/{id}/access` | owner or admin; operator (a member: `403`) | `[MailboxAccess]`: every mailbox, its grants, `readers`, `no_reader`, its own consent to sync (`sync`) and `linked_by`; never the index |
| `PUT /v1/accounts/{id}/access/{user}` | owner or admin (see [Workspaces](#workspaces)); operator: `manage` to members only | `{read, act, send, manage}`, all four → `Grant` |
| `DELETE /v1/accounts/{id}/access/{user}?flags=` | owner or admin, their own flags included; operator | 204; `flags` (`read,act,send,manage`) names what goes, every flag without it |
| `GET /v1/accounts`, `GET /v1/accounts/{id}` | read | the mailboxes the caller holds a grant on, and every mailbox of a team they own or administer (its card); `?workspace=` narrows the list |
| `POST /v1/accounts` | admin (a session counts) | add a mailbox (45 s); `workspace_id` names a team the caller owns or administers, and `sync_consent_version` (the current sync text) gives the team's consent with the link |
| `DELETE /v1/accounts/{id}?confirm=<id>` | admin: a team's owner or admin, a personal mailbox's person, the operator for its own | remove it, with its index; without the id repeated, `400` and nothing removed |
| `POST /v1/accounts/{id}/oauth/start` | admin, manage | start (or restart) its authorization; the flow is the caller's |
| `POST /v1/accounts/oauth/callback` | admin, manage | `{redirect_url}` → `Account` (45 s); only the flow's starter |
| `GET /v1/accounts/{id}/folders` | read, `read` | folders, from the index or from the server |
| `GET /v1/accounts/{id}/sync` | read | `AccountSync` |
| `POST /v1/accounts/{id}/sync` | write, `read` | ask for a pass now → `AccountSync` (202) |
| `PUT /v1/accounts/{id}/sync` | a team mailbox: its owners and admins, signed in; an operator mailbox: unrestricted instance admin key | `{enabled, version?}` → `AccountSync`: the team's consent (on: `version` the current sync text) or the operator's switch; off deletes the index for everyone; `400` for a personal mailbox; `409` to turn on a team mailbox nobody can read |
| `GET/POST/DELETE /v1/me/sync-consent` | see [Sync](#sync-and-consent) | the person's consent to sync |
| `GET/POST/DELETE /v1/me/actions-consent` | see [Actions](#actions) | the person's consent to actions |
| `GET/POST/DELETE /v1/me/send-consent` | see [Sending](#sending) | the person's consent to sending |
| `GET /v1/me/storage` | read | what the mailboxes the caller may read take up in the index; `?workspace=` narrows |
| `GET /v1/me/mcp` | read | `{http}`: whether this server answers MCP over HTTP at `/mcp` |
| `GET/POST /v1/me/apikeys`, `DELETE /v1/me/apikeys/{prefix}` | session | the person's own keys |
| `GET /v1/events` | read | Server-Sent Events of the mailboxes the caller may read; `?workspace=` narrows |
| `GET /v1/events/wait` | read | long poll for new mail; `?workspace=` narrows |
| `GET /v1/messages` | read | search the index; `?workspace=` narrows |
| `GET /v1/messages/{id}` | read | a message, its body fetched from the mail server now |
| `GET /v1/messages/{id}/raw` | read | the original (`message/rfc822`), always as an attachment |
| `GET /v1/messages/{id}/attachments/{path}` | read | one part, by its IMAP section, always as an attachment |
| `PATCH /v1/messages/{id}`, `POST /v1/messages/{flags,move,trash}` | write | actions |
| `POST /v1/messages/send` | send | send a message |
| `GET /v1/sends/{key}?account=` | send | the record of one of the caller's own sends |
| `GET/POST /v1/apikeys`, `DELETE /v1/apikeys/{prefix}` | admin, only with `MAIL_ADMIN_API=true` | instance keys; `mailserver apikey create\|list\|revoke` without `--bootstrap` call these |

`/v1/users/disable` and `/v1/users/delete` carry the address in the body, never in the URL. A
server whose people sign in only through an extension answers `403 not_authorized` to the routes
that sign in with a password, sign up or accept an invitation, change a password or create an
invitation ([Signing in through an extension](#signing-in-through-an-extension)).

There is no current workspace on the server: a session or a key reaches the caller's mailboxes in
every workspace they belong to, and every account (and every storage entry) carries its
`workspace_id`. The console keeps the workspace it shows itself — per tab, remembered for each
person in local storage — and passes `?workspace=ID` to `GET /v1/accounts`, `GET /v1/messages`,
`GET /v1/me/storage`, `GET /v1/events` and `GET /v1/events/wait`, which narrows them to that
workspace's mailboxes, so a view never fetches another workspace's data; a workspace the caller is
not an active member of is `404`, and with `account` as well the account must be in it
(`docs/workspaces.md`, "Choosing the workspace in a request").

An account carries `workspace_id`, `linked_by` (absent for the operator's mailboxes) and `access`,
the caller's own flags as far as their credential's scope reaches; `send.reason` `not_granted` is
a mailbox the caller may not send from.

## Mailboxes

### Adding one

`POST /v1/accounts` takes `{email, provider?, display_name?, password?, imap_host?, imap_port?,
smtp_host?, smtp_port?, smtp_tls?, login_user?, flow?, initial_days?, save_sent_copy?}` and answers
the account and, for OAuth, the authorization to complete. A password account is **tested before it
is saved**: the daemon resolves the hosts, connects and logs in to IMAP (a 30-second budget). A
refused password, an unreachable server and a host on a private network are `400` with different
messages, and nothing is stored.

**iCloud** is generic IMAP on Apple's servers (`imap.mail.me.com:993`, implicit TLS;
`smtp.mail.me.com:587`, STARTTLS), stored as `imap` and shown as `"provider": "icloud"`. With
`"provider": "icloud"` any host, port or TLS field is a `400`. Without `provider`, `@icloud.com`,
`@me.com` and `@mac.com` addresses become iCloud on their own; a custom iCloud+ domain only when
chosen, and it signs in with the account's `@icloud.com` address as `login_user`, because Apple
refuses the custom address as a user name. It connects with an app-specific password: a refused
Apple Account password comes back as `400` at the login test, and the console explains what to use
instead. Changing or resetting the Apple Account password revokes every app-specific password;
there is no route yet to replace a stored password, so the mailbox is removed and connected again.

Gmail in the console connects only with Google sign-in. An app password works from the command line
(an instance key), never for a person: it is a credential without scope or expiry.

### OAuth flows

| Flow | Client | Redirect | Who may use it |
|---|---|---|---|
| `web` | the **web** client, with a secret | `MAIL_PUBLIC_URL + /oauth/return` | sessions, and keys that ask for it |
| `loopback` | the installed client | a listener the daemon opens on `127.0.0.1` (Google) or `localhost` (Microsoft), on a random port, at `/oauth/callback` | keys; sessions only when the console is local |
| `pasted` | the installed client | the same listener; the address the browser ends on is pasted back | keys |
| `device` | the installed client (Microsoft) | none | keys, with `MAIL_MICROSOFT_DEVICE_CODE=true` |

The web clients are `MAIL_GOOGLE_WEB_CLIENT_ID`/`_SECRET` and `MAIL_MICROSOFT_WEB_CLIENT_ID`/`_SECRET`,
registered with the redirect `<MAIL_PUBLIC_URL>/oauth/return` as the
[README](../README.md#mailboxes-and-providers) describes; the installed clients are
`MAIL_GOOGLE_CLIENT_ID`/`_SECRET` and `MAIL_MICROSOFT_CLIENT_ID`. For a **session**, `web` comes
first when the provider has a web client and `MAIL_PUBLIC_URL` is set; `loopback` is offered only when the console is local (`MAIL_PUBLIC_URL` empty or on a loopback
host). A console anywhere else never gets loopback: the redirect would land on the person's own
machine, not the daemon's. `GET /v1/providers` says exactly what each caller can use.

**How the web flow ends.** The provider sends the browser to `/oauth/return?code=…&state=…`, a
console route. The page restores the session, removes the code from the address bar and posts
`POST /v1/accounts/oauth/callback` with `{redirect_url: location.href}` and its own bearer. The
daemon finds the attempt by its `state` **and by its owner**, checks that the caller may still
manage the mailbox, and only then exchanges the code. A `state` that belongs to someone else is
`404`: the attempt is not consumed and its code is never exchanged. This is the defence against
consent phishing, where someone starts a flow in their own account and gets a victim to consent, so
that the victim's mailbox lands in the attacker's account. An `error=` in the address marks the
account `error` and answers `400`.

The loopback listener knows the `state` it waits for: a request with another one gets an error page
and the flow keeps waiting. Removing the mailbox closes the listener at once, and starting a new
authorization replaces the old one: there is one attempt per mailbox.

**A grant counts only once the mailbox opens.** In every flow, after exchanging the code the daemon
checks two things before calling the account `active`:

1. **The scope granted.** When the token response has a `scope` that does not cover what was asked
   (`https://mail.google.com/`; for Microsoft `IMAP.AccessAsUser.All` and `SMTP.Send`), the attempt
   fails. No `scope` in the response refuses nothing.
2. **An IMAP login** with the new token, on the path normal use takes, within 30 seconds. A refusal,
   even after a refresh, fails the attempt (almost always: the person picked another account on the
   provider's page). A server that is down, slow or throttling is not the grant's fault: the account
   becomes `active` and the log carries a warning.

Nothing is stored when either check refuses; a refused re-authorization of a working mailbox does
not switch it off. The work after the provider's answer runs detached from the caller, at most 40
seconds in total, so a code survives a caller that gives up.

**In use.** When the IMAP server refuses the token of an `active` OAuth mailbox, even a freshly
refreshed one, it moves to `needs_reauth` and the console offers to finish authorization again.
Each mailbox remembers which client issued its grant (`installed` or `web`) and refreshes with it,
because a refresh token works only with the registration that issued it. If that client leaves the
configuration, the mailbox goes to `error` saying so, and returns to `active` the first time it is
used after the client is back. An `invalid_client` or `unauthorized_client` on refresh is a server
configuration error (an expired or rotated secret): no mailbox is marked for consent again.

### States and reasons

A mailbox is `pending_auth`, `active`, `needs_reauth`, `error` or `disabled`. When a consent attempt
fails, `state_reason` is one of these fixed texts, never the provider's:

| `state_reason` | When |
|---|---|
| `consent was declined` | the person declined on the provider's page |
| `the authorization attempt expired` | nobody came back within 10 minutes |
| `the provider refused the authorization` | the provider did not exchange the code |
| `the provider issued no refresh token` | the grant came without one |
| `the provider did not grant access to the mailbox` | the granted scope does not cover the mailbox |
| `the mail server refused this authorization for the mailbox` | IMAP refused the new token, even after a refresh |
| `the provider rejected this server's OAuth client` | a wrong or expired client id or secret |
| `the OAuth client that authorized this account is not configured` | the client left the configuration |
| `the authorization could not be completed` | anything else |
| `the mail server rejected the account's access token` | (`needs_reauth`) IMAP refused the token of a mailbox in use |

Every failure a person sees because of the mail server (listing folders, finishing a consent,
testing a password login) logs a `WARN` line with `account`, `provider`, `class` and the redacted
`err` and `server` status line. Never a token, code or password.

## Sync and consent

### Consent to sync

Nothing from a mailbox's messages is stored until its consent to sync is given, and turning sync
off deletes what was stored. Whose consent it is depends on whose mailbox it is:

- **A personal mailbox: its person's**, given once for every mailbox of their personal workspace
  (`users.sync_consent_at` and `users.sync_consent_version`). Connecting and authorizing a mailbox
  is **not** consenting. Only a session gives or withdraws it (`POST`/`DELETE` with any key is
  `403`); a person's key may read it (`GET`); an instance key may not (`403`: there is no person to
  answer for).
- **A team mailbox: its workspace's**, recorded on the mailbox (`sync_enabled_at`,
  `sync_enabled_by`, `sync_consent_version`), which an owner or an admin of the team gives on the
  team's behalf, signed in, to the current sync text — the same text that covers personal
  mailboxes: with the link (`POST /v1/accounts` `sync_consent_version`) or later (`PUT
  /v1/accounts/{id}/sync` `{"enabled": true, "version": "<revision>"}`). Any owner or admin turns
  it off (`{"enabled": false}`), which deletes its index for everyone who reads it. A team mailbox
  also syncs only while someone can read it. The access directory shows the record (`sync`:
  `enabled`, `enabled_at`, `enabled_by`, `version`, `current`, and `migrated` for a consent the
  upgrade copied from the person who linked it, still bound to them until confirmed: their turning
  sync off, or being disabled or deleted, stops it and deletes its index).
- **The revision is checked.** `POST /v1/me/sync-consent` requires `{"version": "<revision>"}`, the
  revision of the text the console showed. Another revision, none, or another field name is `400`;
  so is a team's consent to another revision than the current one. Agreeing again to the same
  revision keeps the first date. The answer has `current_version`: the console compares it with
  `version` to ask again when the text changes, and with its own text's revision so it never agrees
  to a text it did not show.
- **Having agreed to an earlier revision does not stop sync**: eligibility looks only at whether
  the consent stands. A mailbox whose consent was given to an earlier revision keeps syncing, and
  its index keeps being served, until it is turned off.
- **Withdrawing deletes.** `DELETE /v1/me/sync-consent` clears the person's consent and, **in the
  same transaction**, deletes everything sync stored for the mailboxes of their personal
  workspace: messages (with their parts and full-text rows), folders and those mailboxes' events.
  It never touches a team's mailbox (but for a migrated consent still bound to them). Mailboxes,
  credentials and settings stay. After the commit the daemon compacts the full-text index and
  checkpoints the WAL, so the deleted words are gone from the files too. The engine checks
  eligibility inside every transaction that writes to the index, so a batch already on its way
  writes nothing.
- **A mailbox of the operator workspace** has no person to consent: it syncs only when the operator
  switches it on, `mailserver account sync ID on` (`PUT /v1/accounts/{id}/sync {"enabled": true}`,
  unrestricted instance admin key). `off` asks for confirmation and deletes its index. For a
  person's mailbox the route answers `400` to the person and `404` to everyone else: nobody decides
  for its person, not the operator either.
- **The rule the engine reads** (`internal/store/eligibility.go`): the account is `active` and, for
  a personal mailbox, its person is active and has consented; for a team mailbox, the team's
  consent stands and someone reads it; for an operator mailbox, the operator switched it on.

The first sync of a personal or team mailbox reaches back 90 days and then follows new mail. Gmail's All
Mail, Starred and Important are never synced: they are views of other folders.

`SyncConsent` is `{consented, version?, consented_at?, current_version}`.

### An account's sync

Every account in the JSON has `sync`, the `AccountSync` that `GET /v1/accounts/{id}/sync` answers:

| Field | |
|---|---|
| `enabled` | sync is allowed: its person consented; for a team mailbox, the team's consent stands and someone reads it; for an operator mailbox, the operator switched it on |
| `running` | a worker holds the account now |
| `state` | `off`, `initial`, `live`, `backoff` or `stopped` |
| `tier` | `condstore` or `uidpoll`, after the first connection |
| `folders_synced` / `folders_total` | folders whose initial sync is done / folders synced |
| `messages` | messages indexed, a copy in each folder counted |
| `initial_progress` | 0–100 during the initial sync, 100 after |
| `last_synced_at` | the last pass without error (unix) |
| `error_class` | a short class of the last failure (`needs_reauth`, `auth_failed`, `connection_closed`, `rate_limited`, …), never the server's text |
| `next_retry_at` | when an account in backoff tries again |

`POST /v1/accounts/{id}/sync` answers `202`, or `409` when sync is off for the account, it needs
authorizing, the engine has not picked it up yet, or the daemon runs without an engine.

`GET /v1/accounts/{id}/folders` answers from the index once sync is allowed and has listed the
folders: the counts (`messages`, `unseen`) are what is indexed, and each folder has `sync_state`
(`new`, `initial`, `live`, `resync`, `error`, `disabled`). Before that it asks the mail server, and
`sync_state` is absent.

### Consent revisions

Each text a person agrees to has a revision, configured on the daemon:

| Variable | Default | The text |
|---|---|---|
| `MAIL_CONSENT_VERSION_SYNC` | `2026-10-open-sync-3` | what sync stores, under whose agreement (a person's for their personal mailboxes, the team's for a team's), and who reads a team mailbox's index (`web/src/open/SyncText.vue`) |
| `MAIL_CONSENT_VERSION_ACTIONS` | `2026-10-open-actions-2` | the server changing a mailbox when someone allowed to act on it asks (`web/src/open/ActionsText.vue`) |
| `MAIL_CONSENT_VERSION_SEND` | `2026-10-open-sending` | sending from their mailboxes (the open console has no such text) |
| `MAIL_CONSENT_VERSION_KEYS` | `2026-10-open-api-keys` | what a tool holding a new key can do (`web/src/open/KeyTermsText.vue`) |

The defaults are the open console's texts, held to `web/src/open/versions.ts` by
`web/test/contract.spec.ts`: moving a default without a new text fails the build. A server that
serves another console sets the revisions its texts carry. The console sends the revision of the
text it showed, never `current_version`, and the daemon accepts only the configured one. Changing a
revision asks everybody again: a consent to another revision of actions or sending stops counting
(they are refused) until the person agrees to the new text; a consent to another revision of sync
keeps their mailboxes syncing unless they turn it off, and a team's consent to another revision
keeps its mailbox syncing, but a team's consent is only ever given to the current one; a key keeps
the terms it was created under.
Values are printable ASCII without spaces, at most 64 bytes. A console whose texts carry other
revisions than the daemon's sees every agreement refused, and offers only a reload.

## Storage

`GET /v1/me/storage` (read scope, any credential) is what the mailboxes the caller may read take up
in the index: `{mailboxes: [{account_id, workspace_id, email, messages, bytes}], workspaces:
[{workspace_id, mailboxes, messages, bytes}], total: {messages, bytes}, database_bytes?}`, the
mailboxes sorted by address. `workspaces` sums them per workspace, never a mailbox the caller
cannot read, so it is not a workspace's whole usage; `?workspace=` narrows the answer to one.

- **Whose mailboxes**: the ones the caller may **read** — the rule of `GET /v1/accounts`, narrowed
  to the `read` flag, since a grant that only shows a mailbox does not open its index; an instance
  key answers only for the operator workspace's, never a person's. A mailbox never synced, or whose
  index was deleted, is listed with zeros; one whose account needs signing in again keeps, and
  reports, what was already indexed.
- **Per folder copy**: `messages` counts the index rows still in their folder and `bytes` sums their
  `RFC822.SIZE`, so a Gmail message under three synced labels counts three times. `bytes` is the
  mail's size on its server, not what the index keeps of it.
- **`database_bytes`**, only for an instance `owner` signed in: `mail.db` plus `mail.db-wal` on
  disk, everybody's data included.

## Events: SSE and long poll

Both read the event journal (`events`) from a cursor and deliver only events of mailboxes the caller
may **read**, decided in `internal/service` **per event, with access as it stands**: a mailbox
linked or granted while the stream is open appears from its first event, without reconnecting, and
one whose `read` goes (a grant revoked, a membership disabled or removed, the mailbox removed, the
person disabled) stops the moment that change commits. Resuming from an old cursor never yields
events of a mailbox the caller cannot read now. No payload carries a token or a credential;
they may carry subjects and senders, which are the person's own.

`send.finished` is the one exception: it is not the mailbox's but one sender's record, which
several people sending from a shared mailbox each keep apart (`GET /v1/sends/{key}` answers only
the caller's own). It goes only to whoever may read that record: the person who sent it, with the
`send` scope and the `send` flag on the mailbox when it is delivered, whether or not they may read
the mailbox (a stream without `?account=`); an instance key's send, to instance keys with the `send`
scope. A member who reads a shared mailbox never hears of another member's sends.

**`GET /v1/events`** (SSE, no route timeout):

- Each event is `id: <seq>`, `event: <type>`, `data: <Event as JSON>`, with
  `Event = {seq, type, account_id, at, payload}`: the same shape as the long poll's items.
- It resumes after `Last-Event-ID` or `?since=`; the header wins. With neither it starts **from
  now**: the journal is for resuming a stream, not for reading the mailbox.
- Filters: `?account=ID[,ID]` (one the caller does not see is `404`, one they see without `read`
  `403`), `?workspace=ID` and `?types=message.new,…` (an unknown type is `400`).
- `: ping` every 15 s. The credential is **checked again before each batch** and at each ping: an
  ended session or a revoked key receives `event: error` with `{code, message}` instead of the next
  event, and the stream ends. The `code` says what to do:
  - `unauthorized`: the credential no longer works. Do not reconnect with it.
  - `conflict`: the key works, but its restriction lost a mailbox the stream was opened with (its
    person lost `read` on it). Reconnect with the same key: the new stream follows what the key
    still names, and never the lost mailbox, even once `read` comes back.
  - `not_found`: see the next item.
- Access is **checked again** at the same moments. `event: access` (no `id`, like `lagged`) with
  `{"account_id": "…", "read": false}` says a mailbox the stream followed is no longer readable,
  and `"read": true` that one joined. The stream ends with `event: error` and `{"code":
  "not_found", …}` only when its filter can match nothing again — every mailbox named by
  `?account=`, or every mailbox of a restricted key, is gone, or the caller is no longer an active
  member of the workspace `?workspace=` named: do not reconnect with the same filter. A stream of
  everything the caller may read stays open, however little that is (none at all, after the last
  grant is revoked, or before the first), so a mailbox granted or linked later appears on it.
- `event: lagged` (no `id`) comes first when the cursor is older than the journal keeps (7 days,
  and always the latest 10,000 events): what survives follows, but the client must read again the
  state it keeps.
- A slow client loses nothing: the stream waits for it, and what it has not read stays in the
  journal. When a stream ends on its own (the daemon restarted), reconnecting with the last `id`
  as `Last-Event-ID` loses nothing.
- A browser should use `fetch` with the bearer in the header, never `EventSource` (which sends no
  header) and never a token in the URL. The daemon sends `X-Accel-Buffering: no`; a proxy in front
  must not buffer.

**`GET /v1/events/wait?since&timeout&account&workspace`** is a long poll for **new mail**, under the
same rule (mail of a mailbox lost during the wait is left out): `message.new` in an
inbox or a folder without a role (a filter that files straight into a label). A copy in Sent,
Drafts, Trash or Junk is not mail arriving. `timeout` in seconds, up to 55 (absent or `0` is 30).
It answers as soon as something arrives, with everything that arrived until then:
`{timed_out, lagged, events, next_cursor}`, and `next_cursor` is the next call's `since`.

| Type | Payload |
|---|---|
| `message.new` | `{account_id, message_id, folder_id, folder_role, subject, from, internal_date, first_copy, first_inbox_copy}` |
| `message.flags` | `{account_id, message_id, folder_id, flags, seen, flagged, answered, draft, deleted}` |
| `message.moved` | `{account_id, message_id, folder_id, folder_role, new_copy, to, primary_id}`; an action's also has `from_folder_id` and `to_folder_id` |
| `message.deleted` | `{account_id, message_id, folder_id, folder_role}` |
| `folder.changed` | `{account_id, folder_id, name, role, change, count?}`; `change` is `added`, `updated`, `removed`, `initial_done` or `resync_done` |
| `account.state` | `{account_id, state, previous_state, reason?}`, whenever an account's state changes |
| `sync.progress` | the initial sync's progress, rate-limited at the source |
| `send.finished` | `{account_id, key, state, user_id?}`: `user_id` is the sender, absent for an instance key's send; only the sender receives it |

`message_id` is the local id, not the `Message-ID` header. No `message.new` is emitted during the
initial sync; each folder ends with `folder.changed{initial_done}`.

## Messages

The index holds metadata only. A body, an attachment or an original comes from the mail server when
someone asks, is returned, and is **not stored**. Reading **never** marks a message as read, and no
read route writes to the mailbox: every fetch uses `BODY.PEEK` in a folder opened with `EXAMINE`.

| Route | Deadline | Answer |
|---|---|---|
| `GET /v1/messages` | 30 s | `{messages: [MessageSummary], next_cursor?}` |
| `GET /v1/messages/{id}` | 60 s | `Message` |
| `GET /v1/messages/{id}/raw` | 10 min | the original, `message/rfc822`, up to 50 MiB |
| `GET /v1/messages/{id}/attachments/{path}` | 10 min | the part `path` (`2`, `1.2`), transfer encoding undone |

```
Address        = {name, email}
MessageSummary = {id, account_id, folder_id, folder_role?, folder_name, subject, from: [Address],
                  to: [Address], date, internal_date, size, seen, flagged, answered, draft,
                  has_attachments, copies}
Message        = MessageSummary + {cc, bcc, reply_to: [Address], message_id, in_reply_to,
                  references: [string], parts: [{path, mime_type, filename?, size, disposition?,
                  content_id?, is_attachment}],
                  body: {text?, html?, html_unsafe: true, truncated, charset_fallback}}
```

`id` is the local row id, never the `Message-ID` header. Dates are unix seconds; `date` is the
`Date` header and `internal_date`, when the server received the message, orders the list. `copies`
counts the other live rows of the same message in the account (Gmail's other labels).

**Search** (`GET /v1/messages`): `account`, `folder` (an id from the index's folder list), `q`,
`from`, `unseen`, `flagged`, `has_attachments` (`true`/`false`), `since`, `until`, `cursor` and
`limit` (1–100, default 50).

- Without `folder`, each message appears **once**; with it, that folder's rows. Without `account`,
  every mailbox the caller sees.
- Newest first; `next_cursor` is opaque and present only when there is more. Mail arriving between
  two pages neither repeats nor skips anything.
- `q` matches subject, sender and recipients, **never the body**, which the index does not have.
  Each word is quoted, so nothing typed becomes a full-text operator; the last word matches as a
  prefix, and accents are folded. More than 256 characters or 16 words is `400`.
- `since` and `until` take a date (`YYYY-MM-DD`, UTC, a whole day for `until`), an RFC 3339 time or
  unix seconds.
- A search reads only the database.

**Reading** (`GET /v1/messages/{id}?format&max_bytes`): `format` is `text`, `html` or `both`
(default); `max_bytes` bounds **each** body, 1 byte to 5 MiB (default 1 MiB). Each body part is
fetched on the **engine's interactive connection**, so a mailbox never uses more than three
connections and concurrent reads queue on it. Decoding is lenient: an unknown or wrong charset falls
back to UTF-8 or Windows-1252 and says `charset_fallback`; a cut body says `truncated`. The HTML comes
back as it was, unsanitized, with `html_unsafe: true`: whoever shows it must isolate it. A gone row
or UID is `404`; a folder being read again (`UIDVALIDITY` changed) is `409`, try again; an account
needing authorization is `409`; an unreachable server is `500` with `upstream: …`; the provider's
connection limit is `429`. Search keeps answering from the index in every case.

**Originals and attachments** are fetched whole into a temporary file (`<MAIL_DATA_DIR>/tmp`, mode
`0600`), the connection is released, the credential is checked again, and only then does the
response start; the file goes when the response ends, and the daemon clears any left behind when it
starts. Limits: 50 MiB for an original, 50 MiB encoded for an attachment (or the provider's sending
limit, if larger), decided from the index before any fetch. At most four downloads at once per
person (or per instance key), and `MAIL_DOWNLOAD_SPOOL_MAX_BYTES` (1 GiB) for the whole daemon;
beyond that, `429`. Every file response is `Content-Disposition: attachment` with a sanitized name,
`nosniff`, `Content-Security-Policy: sandbox; default-src 'none'` and `Cache-Control: no-store`, and
its `Content-Type` comes from a list of types browsers show passively; anything else (HTML, SVG, XML,
scripts, `multipart/*`) is served as `application/octet-stream`.

## Actions

Mark read or unread, star or unstar, archive, move, trash, and undo a move. The daemon changes the
message **on the mail server**, so the change shows in every other app, and the index follows what
the server confirmed. Nothing is ever deleted for good: the trash is as far as Mailie takes a
message.

### Consent to actions

Changing a mailbox is a new use of it, so actions have their **own** consent, separate from sync:
`users.actions_consent_at` and `users.actions_consent_version`, revision
`MAIL_CONSENT_VERSION_ACTIONS`. `GET /v1/me/actions-consent` reads it (a person's key may; an
instance key may not). `POST {"version": …}` and `DELETE` are the person's, signed in. Withdrawing
deletes nothing and stops actions at once: every action checks the consent before connecting and
again, on the connection, before each command that changes the mailbox. `ActionsConsent` has the
shape of `SyncConsent`. Without sync there is no index, and without an index no message to act on.

### Who may act

Decided in `internal/service`, before any connection, in this order:

1. The `write` scope; a `read` key gets `403`.
2. Every message must be readable by the caller (`read` on its mailbox); one they cannot read, or
   one that does not exist, makes the whole request `404`. All in one account, 1 to 100 ids.
3. **A person's mailbox**: the caller (their session or a key they created) must hold `act` on it,
   and **their own** consent to actions must name the current revision — whoever linked the
   mailbox. Without `act`, `403`; without consent, or with an old one, `409`.
4. **A mailbox of the operator workspace**: an instance key with `write`.
5. The account must be usable: `needs_reauth`, `pending_auth` and `disabled` are `409`.

### Routes

Write scope, 30 s, a body of at most 64 KiB; an unknown field is `400`.

| Route | Body | |
|---|---|---|
| `PATCH /v1/messages/{id}` | `{seen?, flagged?}` | one message |
| `POST /v1/messages/flags` | `{ids, seen?, flagged?}` | several messages of one account |
| `POST /v1/messages/move` | `{ids, to}` | `to` is `"archive"`, `"inbox"` or a folder id |
| `POST /v1/messages/trash` | `{ids}` | to the account's trash folder |

Each answers `{messages: [MessageSummary], removed: [id]}`: the rows as the index has them **after**
the action, a moved row keeping its id, and in `removed` the ids that left the index (archiving on
Gmail, where All Mail is not synced, or a message whose new UID the server did not say). An id in
`removed` is still accepted by `POST /v1/messages/move` for 10 minutes, which is how undoing a Gmail
archive works. Nothing is optimistic: the answer is what the server confirmed.

Each account has `actions: {archive, trash}` in its JSON: whether there is somewhere to archive to
(Gmail's All Mail, or a synced `archive` folder) and a trash folder. `archive_reason:
"all_mail_hidden"` says a Gmail mailbox hides All Mail from IMAP, which archiving needs.

**Destinations.** `archive` on Gmail moves inbox copies to All Mail; elsewhere, to the synced
`archive` folder (`409` without one). `inbox` is the inbox. A folder id must be of the same account,
selectable, synced and not Sent, Drafts, Starred or Important; the trash is refused there (`400`,
"use the trash action"), because it has a route of its own that an MCP client can hold for
confirmation. `trash` without a trash folder, or of a message already in it, is `409`.

### On the server and in the index

- Everything runs on the engine's **interactive connection**, as reading does.
- Flags are `UID STORE +FLAGS/-FLAGS` with `\Seen` and `\Flagged`; the index records the flags the
  server echoed. On Gmail a flag belongs to the message, so every label copy in the index gets it.
- A move is `UID MOVE`. Without `MOVE`, `COPY` + `STORE \Deleted` + `UID EXPUNGE` of **only the
  moved UIDs**, which needs `UIDPLUS`; without either, the move is refused (`409`) before any
  command. There is no other `\Deleted` or `EXPUNGE` in the daemon.
- Undoing a Gmail archive copies the message from All Mail back into the inbox, never moves it:
  on Gmail, copying adds the label and moving out of All Mail would delete the message.
- A command that was sent is waited for (up to 30 s) and recorded, even when the caller gives up.
- A row moved to a synced folder whose new UID is known is **rewritten in place** and keeps its id.
  The new UID is known from the `COPYUID` of one message; for several, the daemon reads the
  destination back and pairs each row with its message, because `COPYUID` does not say which went
  where. A row moved somewhere unsynced, or whose UID is unknown, leaves the index, and the
  destination's next pass indexes it as moved.
- An action never emits `message.new` or `message.deleted`: it emits `message.moved`. For 10 minutes,
  in memory, the index remembers what moved, so a sync pass racing an action neither resurrects the
  source row nor announces the message as new mail.

## Sending

Plain-text messages, replies and forwards with attachments, sent through the account's SMTP server.
Mailie does not keep what is sent: the text and attachments are on the server only while the send
runs, and the copy stays in the provider's Sent folder. The open console has no compose screen;
sending is an API feature.

### Consent to sending

Its **own** consent, like actions: `users.send_consent_at` and `users.send_consent_version`, revision
`MAIL_CONSENT_VERSION_SEND`. `GET /v1/me/send-consent` reads it; `POST {"version": …}` and `DELETE`
are the person's, signed in. Withdrawing deletes nothing and stops sending at once: every send checks
the consent when it is accepted and **again right before each connection to the SMTP server**. The
open console never asks for this consent, so on a server running it a person's mailbox does not
send unless that person agrees through the API with their session.

### Who may send

1. The `send` scope (sessions have it; personal keys never do).
2. `confirm: true` in the message. Without it, `400` before anything else: nothing is reserved or
   dialed. No flag or test relaxes this.
3. The account must be visible to the caller (`404` otherwise).
4. **A person's mailbox**: the caller must hold `send` on it, with **their own** consent to sending
   at the current revision. Without `send`, `403`; without consent, `409`. A reply or a forward
   also needs `read` on the original.
5. **A mailbox of the operator workspace**: an instance key with `send`.
6. The account must be usable (`409` otherwise).

Each account's JSON has `send: {available, reason?, from_name?}`: whether it can send **for this
caller**, consent aside. `reason` is `needs_reauth`, `pending_auth`, `disabled`, `no_smtp` or
`not_granted` (no `send` flag, or a credential that does not send). `from_name` is the caller's
own profile name: a message goes out under the name of whoever sends it.

### `POST /v1/messages/send`

`multipart/form-data`, 3 minutes: first a `compose` part (JSON), then zero or more `attachment`
parts, each a file. An `Idempotency-Key` header (1 to 128 letters, digits, `-`, `_`, `.` or `:`) is
**required for a session** and optional for a key; without it, a key's sends are keyed by
`<compose_hash>/<minute>`, so repeating the same message within the minute is a replay, not a second
send.

```jsonc
{
  "account_id": "acc_…",
  "to":  [{"name": "Bea Lima", "email": "bea@example.org"}],
  "cc":  [], "bcc": [],
  "subject": "",
  "text": "plain text, UTF-8",
  "in_reply_to": 1284,                                       // optional: a message the caller can read
  "forward_of": 1290,                                        // optional, not with in_reply_to
  "forward_attachments": [{"message_id": 1290, "path": "2"}], // optional: parts fetched from the server
  "confirm": true
}
```

Limits, each a `400` before any connection: 1 to 100 recipients across `to`, `cc` and `bcc`; plain
addresses of at most 254 bytes; no line breaks or control characters in names and subject; a subject
of at most 998 bytes; text up to 1 MiB; up to 100 attachments; the estimated message within the
provider's size limit. Error messages cite positions (`to[2]`), never addresses. A person sends at
most 200 messages a day, and each account at most its provider's rate per minute (`429`).

- The sender is the account's address, under the **sender's** profile name, whoever linked the
  mailbox (the address alone for an instance key).
- A reply sets `In-Reply-To` and `References` from the original, and `Re: ` once. After it is sent,
  the original gets `\Answered` only if the sender holds `act` on it and allowed actions.
- A forward adds `Fwd: ` once; `forward_attachments` are fetched from the server when it is sent.
- An attachment's type is **sniffed**, and anything outside the passive list goes out as
  `application/octet-stream`; its name is sanitized.
- Attachments and the assembled message are spooled to `0600` files under `<MAIL_DATA_DIR>/tmp`
  and removed when the send ends, however it ends.

**The answer.** A send that was attempted answers `200` with
`{state, message_id, sent_at?, replayed, reason?, rejected?}`. `state` is:

- `sent`: the server accepted it. `replayed: true` means the same key already sent this message and
  nothing went out now.
- `failed`: nothing was delivered; the same key may try again. `reason` is a fixed word:
  `recipients_refused` (with `rejected`), `too_large`, `refused`, `auth_failed` (the account's state
  does not change), `auth_unsupported` (the server does not allow this login for sending, such as SMTP
  AUTH turned off on a Microsoft 365 mailbox), `needs_reauth`, `rate_limited`, `temporary`,
  `unreachable`, `stopped`.
- `unknown`: the message **may** have been delivered (`after_data`: the connection dropped or went
  silent after the body started; `interrupted`: the daemon stopped). It is **never retried**.

**Idempotency is reserved before dialing**, in an immediate transaction. The same key with the same
message: `sent` replays the stored answer, `sending` is `409` (in progress), `unknown` is `409`
(check Sent before sending again). The same key with another message is `409`, and so is a key
another person used on the same shared mailbox: their send is never replayed to anyone else. `failed` frees the
key. When the daemon starts, any send still `sending` becomes `unknown`, never `failed`.

**At the SMTP server.** One connection per send, an account's sends in series, the `Message-ID` and
`Date` fixed before the first attempt. A server that says "later" (4xx) or does not answer **before
the body goes** is retried up to three times within the request. **A connection that drops or goes
silent after the body starts is `unknown`, never retried.** A 4xx or 5xx at the end of `DATA` is a
refusal (RFC 5321): `failed`, and not retried either. A refused OAuth token is refreshed once; a
second refusal is `auth_failed` and leaves the account as it is, because whether the grant died is
IMAP's to say.

**`unknown` reconciled from Sent.** Gmail and Microsoft file the copy themselves: when the sync of
the `sent` folder indexes a message with the `Message-ID` of an `unknown` send, the send becomes
`sent` and `send.finished` is emitted. A generic IMAP account has no such source and stays `unknown`.

**The Sent copy.** Never `APPEND` for Gmail and Microsoft, which file it themselves. For a generic
IMAP account with `save_sent_copy`, after `sent` the daemon looks for the `Message-ID` in the Sent
folder and, if the server did not file it, appends **the exact bytes sent** with a `Bcc:` header in
front, marked `\Seen`.

**`GET /v1/sends/{key}?account=`** (send scope; still readable after withdrawing consent) answers,
for one of the caller's own sends (another person's on a shared mailbox is `404`),
`{account_id, idempotency_key, state, message_id, reason?, attempts, recipients, sent_copy,
created_at, updated_at, sent_at?}`. A derived key contains `/`: encode it (`%2F`).

**What is kept**, per send, for 30 days after its last change: account, key, a keyed hash of the
message, `Message-ID`, state, attempts, reason, how many recipients, the Sent copy's state and who
asked. **No subject, address or text.**

## API keys

A person creates their own keys in **API keys & MCP**, for a tool (an assistant over MCP, a script
over REST) to read the mailboxes they choose and, with `write`, act on them. Creating the key is
their agreement to what a tool holding it can do. Only the person **signed in** lists, creates and
revokes their keys: any key, the new one included, gets `403` on these routes. No key creates a key.

| Route | Body | Answer |
|---|---|---|
| `GET /v1/me/apikeys` | | `[PersonalKey]`, newest first, revoked and expired ones included |
| `POST /v1/me/apikeys` | `{name, scope, account_ids?, ttl_days?, terms_version}` | `201` `PersonalKey` + `{key}` |
| `DELETE /v1/me/apikeys/{prefix}` | | `204`, also when already revoked |

```jsonc
{
  "prefix": "0a0b0c01",            // names the key; not a secret
  "name": "Claude Code",
  "scope": "read",                 // "read" | "write"
  "account_ids": ["acc_…"],        // the mailboxes it reaches; [] means all, including future ones, unless restricted
  "restricted": true,              // created for chosen mailboxes; with all of them removed it is revoked
  "created_at": 1790000000,
  "expires_at": 1797776000,
  "last_used_at": 1790003600,      // absent if never used; one-minute resolution
  "revoked_at": 1790000000,        // absent while alive
  "terms_version": "2026-10-open-api-keys"
}
```

- `name`: 1 to 120 characters. `scope`: `read` or `write`. `ttl_days`: 30, 90 or 365 (default 90).
- `account_ids`: mailboxes the person may **read**; one they do not see is `404`, one they see
  without `read` `403`, and nothing is created. A key reaches only what its person can, at each
  request: with `read` on a mailbox lost, the key no longer reaches it.
- `terms_version` must be `MAIL_CONSENT_VERSION_KEYS`; another is `409`. A key keeps working under
  the terms it was created with.
- At most **20 live keys** per person; the 21st is `409`.
- The secret appears **once**, in the `POST` answer (`Cache-Control: no-store`).
- The list shows every key that acts as the person, so they can revoke it, including one an
  administrator issued for them before personal keys existed (`terms_version` empty). Such a key
  works on no route, REST or `/mcp`.
- A key restricted to mailboxes is **revoked** when its last mailbox is removed, rather than becoming
  a key for every mailbox. Losing `read` on a mailbox takes it out of the person's keys in the same
  transaction, so a key made for it alone is revoked too, and does not come back with a new grant; a
  caller still holding what such a key authenticated as (an MCP session, a stream) is refused at
  its next re-check.

The console shows the MCP address (`<origin>/mcp`) and the Claude Code command only when
`GET /v1/me/mcp` says the server answers there.

## Running the console locally

Prerequisites: Go 1.27, Node 24, and a `.env` with `MAIL_CREDENTIAL_KEY_HEX` (see the README).

```sh
echo 'MAIL_PUBLIC_URL=http://localhost:5174' >> .env
make build
./bin/mailserver user invite --bootstrap --role owner --email you@example.com   # daemon stopped
make run                     # the daemon, on 127.0.0.1:8080
make web-install web-dev     # another terminal: http://localhost:5174, /v1 and /mcp proxied
```

Open the invite link with `localhost`, never `127.0.0.1`: Google's web client matches host and port
exactly, and Entra does not accept `127.0.0.1`. The dev server's port is fixed (`strictPort`)
because a registered redirect names it.

- Generic IMAP needs no configuration. A server on `localhost` or a private network (a local
  Dovecot) needs `MAIL_ACCOUNT_ALLOW_PRIVATE=true`.
- iCloud needs only the app-specific password.
- Gmail and Microsoft without web clients use the `loopback` flow with the installed clients: the
  console shows the provider's link, you consent in a new tab, the daemon receives the redirect on
  its own listener, and the console notices through the `account.state` event. The browser must be
  on the daemon's machine.
- With web clients, they use the `web` flow, registered for `http://localhost:5174/oauth/return`.

To serve the build from the daemon instead:

```sh
make web-build
MAIL_WEB_DIR=web/dist MAIL_PUBLIC_URL=http://localhost:8080 ./bin/mailserver serve
```

The Google web client then needs `http://localhost:8080/oauth/return` registered as well. To run the
daemon on another port, set `MAIL_HTTP_ADDR=127.0.0.1:8081` and point the dev server at it with
`MAIL_DEV_TARGET=http://127.0.0.1:8081 make web-dev`.

`make web-check` runs the console's typecheck, tests and build; it is not part of `make check`, and
CI runs it in a job of its own. `web/README.md` describes the tests and the optional browser QA.

## Not implemented

- Drafts kept on the server, composing HTML, the MCP tools that send, and webhooks. Permanent
  deletion does not exist and will not.
- Searching message bodies.
- Self-service sign-up, email verification, password recovery by email (the operator resets a
  forgotten password: [Passwords and sign-in](#passwords-and-sign-in)), passkeys.
- A screen for the server's own people: inviting someone to the server, disabling or deleting a
  person (`POST /v1/users/*`); the routes and the command line exist. A team's people have theirs
  (Members).
- OAuth for MCP clients: `/mcp` takes a key as a bearer.
- Renaming or pausing a mailbox, and replacing the stored password of a password mailbox.
- Periodic cleanup of expired sessions (they stay with the account; unused invitations are swept).
- The device-code flow in the console.
