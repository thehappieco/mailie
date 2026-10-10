# The server console and its API

The console is where a person signs in to **their** account on a Mailie server and connects their
own mailboxes: Gmail, Microsoft 365/Outlook, iCloud Mail and generic IMAP, in their personal
workspace or in a team. The open console (`web/`) has five sections: **Mailboxes**, **Members**
(the people of the team shown, or making a team), **API keys & MCP** (the keys of the workspace
shown, for its owners and admins, and how to connect a tool over MCP), **Storage** and **Account**
(name, password, sessions, what they allow the server to do, sync and actions, and the API keys
they created). No mail is read or written there; a tool does that, with a key.

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
workspace; the API keys section is the workspace's), a few sentences it words its own
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
load the person, so no edition keeps a session of its own. A sign-in that opened or made the
person's account key keeps it first (`keepAccountKey`, `state/accountVault.ts`) and then adopts:
`adoptSession` begins the session keyed when this browser holds the key of the person the reply
names, as a restore does, and wipes a key of anyone else; one that never adopts settles the key it
kept (`settleRecord`). `signedOut` is called once the person has signed out on purpose, after the
session ended in this browser and the server was told, never when one expires, is refused or ends
in another tab; an edition may navigate away there. A person whose `has_password` is `false` is not
offered to change a password.

Such an edition may also bring its own **step-up** (`stepUp(reason)`), in place of the password
dialog: `state/stepUp.ts` calls it when a flow needs a fresh step-up, once for every flow waiting
then, with what the first of them asked it for (`'link'`, `'grant'` or `'key'`). It resolves once
the session's step-up time is fresh, recorded with `steppedUp` (`state/session.ts`) from what its
own route answered, and the flows go on; it rejects with `StepUpCancelled` when the person gives
up, which stops them without a word, or with any other error to stop them with that one. A prompt
settled meanwhile (a sign-out, another person) is not settled again by its late answer. It may be
called after awaits, outside the click that started the flow, so a window it opens needs a click of
its own. `copy.stepUpHint` words the sentence beside a guarded write, which the core's names the
password; the edition's describer (`addDescriber`) words the `no_account_key` and `not_enrolled`
failures (the core's say to sign in again here, and, for `not_enrolled`, to ask the administrator
for a reset link). The open edition sets none of these.

The hosted service's app is such an edition, kept in a private repository: it compiles `web/src`
from source and adds reading and writing mail. The direction is one way. Nothing under `web/src`
imports from outside it or through an alias (a package of the console's `dependencies`, such as
the kit `web/src/crypto/mailie.ts` builds the key scheme on, is not an alias), and the open console
names nothing of the hosted service (`web/test/editions.spec.ts` reads every file under `web/src`
and `web/public`, and `web/index.html`, for the names in `web/test/hosted.ts`, setting aside only
the kit's package name in an import; the build fails on an output file that has one). Keep both in
mind when you change the core: an edition you cannot see builds on it.

## People and credentials

### Sessions

A session is an opaque **bearer token, never a cookie**: 32 random bytes in base64url (43
characters, without a dot, which is how the daemon tells it from an API key `<prefix>.<secret>`),
stored as SHA-256, with an absolute lifetime of 14 days and no sliding renewal (one started through
an extension may be given less, never more: see
[Signing in through an extension](#signing-in-through-an-extension)). The browser keeps
it in memory and an encrypted copy in IndexedDB under a non-extractable AES-GCM key, tells other
tabs over a `BroadcastChannel` when it signs out, and drops it on any `401`, wiping the account key
it kept with it ([The console's half](#the-consoles-half)). Routes for a person
(`/v1/auth/*`) refuse API keys.

### Passwords and sign-in

The server never receives a password (the key scheme, [`key-scheme.md`](key-scheme.md), sections
5 and 12; its threat model, [`key-scheme-threat-model.md`](key-scheme-threat-model.md)). The
browser prepares the password with the platform's profile, stretches it with Argon2id under a salt
and parameters the server hands out, and splits the result: the **auth key**, which it sends and
the server keeps only as a hash (Argon2id at an API key's cost, 19 MiB), proves who the person is;
the **wrap key**, which never leaves the browser, wraps the person's **account key**, an X25519 key
pair the browser made at enrolment. The server keeps the public half, written once, the account key
wrapped under the password and under a **recovery code** (150 bits, shown once at enrolment and
replaced at every recovery), and a hash of the recovery code's proof. A new password has at least
twelve code points, which the console checks before it derives anything; a password being
presented has no minimum.

- **The challenge.** `POST /v1/auth/challenge {email}` answers `{salt, kdf}`, and nothing more: an
  enrolled person's own, and for any other address (unknown, disabled, a person with no password,
  a person from before the key scheme who never enrolled) the address's salt under the server's
  salt key and the default parameters, which is also what an account at its target stores, so the
  answer does not say whether an address has an account. The salt key
  is made at the daemon's first start and kept in the database sealed like a credential
  (`meta.kdf_salt_key`, under `MAIL_CREDENTIAL_KEY_HEX` or the KMS key). The browser refuses
  parameters or a salt outside the platform's bounds before it derives anything.
- **Signing in.** `POST /v1/auth/login {email, auth_key}` answers the session (its step-up time
  now), and the password wrap, which the browser opens with its wrap key: a wrap that does not
  open after the auth key was accepted is a security error, not a wrong password. When the account
  is not at its target (its address changed, or the default was raised) the answer adds `rederive
  {salt, kdf, ticket}`: the browser derives the same password under it and finishes with
  `POST /v1/auth/password/finish`, sending again the auth key it signed in with
  (`current_auth_key`), and no session ends. An unknown address, a disabled person, a
  person who has not enrolled and a person with no password cost the same work as a wrong auth key
  and get the same answer (`unauthorized`): an auth key is checked against a dummy verifier in all
  of them. At most two checks of people's secrets run at once. No route takes a password in clear.
- **Signing up.** An invitation's link opens the sign-up: `POST /v1/auth/signup/open {invite,
  email}` checks the invitation as the sign-up will and answers `{salt, kdf, seal_id}`, the
  address's target and the person's **seal id**, the UUID every wrap and grant binds them by,
  drawn once per invitation. The browser makes the account key and a recovery code, wraps the key
  under that seal id, shows the code once, and sends `POST /v1/auth/signup {invite, email, name,
  seal_id, auth_key, kdf, public_key, password_wrap, recovery_wrap, recovery_proof}`. The server
  checks the invitation, that the seal id is the one it answered (`409` otherwise: open it
  again), that `kdf` is its default (`409` otherwise: derive again), the public key (a valid
  X25519 key, not of low order) and the wraps' shape (61 bytes starting with `0x02`).
- **Changing the password** is two steps: `POST /v1/auth/password/begin {current_auth_key}`
  answers the current password wrap, the target and a ticket only to the current auth key verified
  in that request (a session alone gets nothing); the browser wraps the same account key under the
  new password and sends `POST /v1/auth/password/finish {ticket, current_auth_key, auth_key, kdf,
  password_wrap}`, the same current auth key again.
  Every session of the person ends, and the reply is the one this browser goes on with; the account
  key, the grants and the recovery code do not change. A person without a password has no current
  one to prove, and the console does not offer the change (`user.has_password` is `false`).
- **Recovery.** `POST /v1/auth/recover/open {email, recovery_proof}` answers the recovery wrap, the
  seal id, the public key, the target and a ticket, only to the proof verified in that request;
  `POST /v1/auth/recover/finish {ticket, current_recovery_proof, auth_key, kdf, password_wrap,
  recovery_wrap, recovery_proof}`, with the proof that opened the recovery sent again
  (`current_recovery_proof`; `recovery_proof` is the new code's), stores a new password and a new
  recovery code over the same account key, and ends every session. The console then signs in with
  the new password; when that sign-in fails (too many attempts, the network), the recovery is done
  all the same, and the form says so and asks for the new password rather than for another
  recovery, which would only replace the code it just showed.
- **Replacing the recovery code.** `POST /v1/auth/recovery {current_auth_key, recovery_wrap,
  recovery_proof}` replaces the recovery code of a signed-in person, with their current auth key
  verified in that request under the account's sign-in limit, as `password/begin` is: a session
  alone, however recent its sign-in, sets no secret of its person's, since a recovery code it could
  set would be a password it could set, through a recovery. A wrong key is `403`
  (`not_authorized`) and changes nothing. It ends the recoveries opened with the old code.
- **Step-up.** `POST /v1/auth/stepup {auth_key}` proves the session's own person again: it checks
  the auth key of that person only and refreshes that one session's step-up time
  (`{authenticated_at}`), as a sign-in, a sign-up or a reset sets it. The routes that write a
  mailbox key or a grant for someone else refuse a session whose step-up time is more than ten
  minutes old, or none, with `403` (`not_authorized`) and nothing written
  ([`key-scheme.md`](key-scheme.md) section 11): linking a mailbox (`POST /v1/accounts`), giving
  "read" with a grant to someone else (`PUT /v1/accounts/{id}/access/{user}` with `grant`),
  supplying the key (`PUT /v1/accounts/{id}/grants/{user}`), a mailbox's first key and a personal
  mailbox's new key (`POST` and `PUT /v1/accounts/{id}/mailbox-key`); see
  [Mailbox keys](#mailbox-keys). Flags alone, `read` by the flag to a person without an account key
  or on a mailbox without a key included, need none. A sign-in counts for its first ten minutes.
  `GET /v1/auth/me` reports the session's `authenticated_at` (0: none), so the console can ask for
  the password again before it calls such a route rather than after a refusal.
- **Tickets** (re-derivation, password change, recovery) are single-use, valid ten minutes, stored
  as SHA-256 and bound to the person; for a password change and a re-derivation, to the session and
  the auth key that earned the ticket, and for a recovery to the recovery proof that opened it, each
  kept as SHA-256: whoever saw only the answer that carried the ticket cannot finish it. A ticket
  that is not one costs no Argon2id to refuse, and a refused one stays its own. The schema still
  admits the upgrade's purpose, `enrol`, which nothing issues now and no ceremony takes (below). A
  ceremony that changes the password (a change, a re-derivation, a recovery, a reset) spends every
  other ticket of the person; replacing the recovery code spends only the recoveries opened with the
  old code, and a password change or re-derivation in flight goes on, since the password it proved
  has not changed.
- **The upgrade, removed.** The release that brought the key scheme let a person who signed up
  before it, with a password hashed on the server and no account key, send that password in clear
  **one last time** (`POST /v1/auth/upgrade/login` and `/enrol`, announced by the challenge's
  `upgrade: true`) and enrol with it. The next release removed both routes and the `upgrade`
  answer ([`key-scheme.md`](key-scheme.md) section 12.7): they are not found now, and the server
  checks no password in clear. A person who did not sign in during that release has not enrolled:
  their challenge answers their address's target, and every sign-in of theirs fails as a wrong
  password does, after the same work. Their old hash stays in the database (no migration), and no
  sign-in reads it, until a reset invitation, below, enrols them and clears it: that is their way
  back.
- **A lost password and recovery code.** The operator prints a **reset invitation** with the
  daemon stopped: `mailserver user password --bootstrap --email X [--force]`. It sets no password:
  it prints a single-use link, `<MAIL_PUBLIC_URL>/#reset=…&email=…`, valid seven days (the code in
  the fragment, stored as SHA-256), and changes nothing until it is used. The person's browser asks
  `POST /v1/auth/reset/open {reset, email}` what the new password is derived under and the wraps
  bound to, `{salt, kdf, seal_id}`: the account's target, which the reset stores (not what a
  challenge answers an account off its target), and the person's seal id. It then makes a new
  account key, password and recovery code and sends `POST /v1/auth/reset {reset, email, auth_key,
  kdf, public_key, password_wrap, recovery_wrap, recovery_proof}`; in one transaction the server replaces the public key (the one replacement of a
  key written once), deletes every grant sealed to the old one, ends every session and signs this
  browser in. A reset takes "read" from the person on every mailbox that has a key, so the command
  refuses, naming the mailboxes, while the person is the last reader of a team mailbox, a team they
  are alone in included (closing a person leaves that team out, since it goes with them; a reset
  leaves it), unless `--force`, which the invitation records; one issued without force checks
  again when it is opened and when it is used (`409`).
  A disabled person's link works only once they are enabled again; disabling a person deletes
  their invitations and tickets. There is no route that makes one, by design. `--email -` reads
  the address from standard input. It is also how a person who signs in through an extension, and
  has no password, is given one, and how a person from before the key scheme who never enrolled
  signs in again: completing it enrols them.

### The console's half

What the browser does in each ceremony is `web/src/crypto/account.ts` (derivations, wraps and the
recovery code, over `web/src/crypto/mailie.ts`), and the order of its requests and what it keeps is
`web/src/state/account.ts`:

- **Argon2id runs in the kit's worker** (`kdf.worker.js`, which the build emits beside the page), so
  the tab stays responsive for the seconds a derivation takes; a browser without workers derives on
  the page. The policy's `worker-src 'self'` allows that one worker and nothing else (no `blob:`,
  no WebAssembly). The console refuses a salt or parameters outside the platform's bounds before it
  derives, and a new password under twelve code points before it asks the server anything.
- **The account key at rest** (`web/src/state/accountVault.ts`): the kit's key at rest under
  Mailie's profile, in IndexedDB beside the session's record, one per browser profile, bound to the
  person's seal id and public key. It is opened only for the person `GET /v1/auth/me` names (a
  record of anyone else is wiped), and wiped at sign-out and whenever a page of the console finds
  no valid session (expired, revoked, ended by a password change, a recovery or a reset); an
  edition whose key outlives a session that merely expires says so (`Edition.accountKey`), and
  keeps it only past a session a page of the console established as expired by the server's clock
  (its expiry timer, or a refusal whose answer's `Date` is past `expires_at`, asked again when the
  refusal carried none), marking the record until a session begins with it; a refusal before the
  expiry, a session given up on unchecked, and no session record beside an unmarked key wipe it
  there too. Nothing
  runs while the console is closed: a browser closed while signed in keeps the record until the
  console next runs there, whatever happens to the session meanwhile ([`key-scheme-threat-model.md`](key-scheme-threat-model.md)
  section 4.6). The page asks its own copy first, so a browser that refuses IndexedDB, or opens it
  and refuses the write, keeps the key for the page only.
- **The recovery code is shown once**, after a sign-up, a recovery and a reset link, and after the
  person replaces theirs, in a dialog that stays until they say they saved it
  (`components/RecoveryCodeDialog.vue`); nothing keeps it once it closes.
- **A new recovery code** (`components/AccountPanel.vue`) asks for the password every time,
  derives the current auth key under the account's own salt and parameters, and sends it with the
  new wrap and proof; the password is never sent.
- **The step-up prompt** (`components/StepUpDialog.vue`, mounted once for the page by
  `components/StepUpPrompt.vue`) is asked for before every call the step-up guards
  ([Mailbox keys](#mailbox-keys)): linking a mailbox, giving Read with a grant, handing a
  mailbox's key over, and its first or a new key. `state/stepUp.ts` asks first, over whatever
  dialog is open, unless the session's `authenticated_at` leaves 30 seconds of the ten minutes (two
  minutes for a link, which the server checks again after its login to the mail server, up to 30
  seconds later); the flow goes on once the password is proved (`POST /v1/auth/stepup`), and stops
  without a word, sending nothing, when the dialog is closed. An edition with a step-up of its own
  (`Edition.stepUp`, [Editions](#editions)) is asked instead, and the dialog is not drawn. A `403`
  after which the step-up is no longer fresh was the step-up going stale on the way: the prompt is
  shown again and the call made once more, with keys and grants sealed anew; any other `403` is
  the call's. The console judges
  the session's `authenticated_at` by the server's clock, as the `Date` of the server's answers
  gives it (`state/connection.ts`), never by its own: a browser whose clock runs ahead would
  otherwise find a step-up it just made already old.
- **The links.** An invitation (`#invite=…&email=…`) starts with `signup/open`; a reset link
  (`#reset=…&email=…`) with `reset/open`, which says before any password is typed that a link is
  not valid or would take a team mailbox's last reader. Both codes leave the address bar at once.
- **No password is sent**, in any request, whatever a challenge answers: the sign-in derives and
  sends the auth key alone, and ignores an `upgrade` member as it ignores any member it does not
  know, so the sign-in form says, as the forms that choose a new password do, that the password
  never leaves the browser. The console keeps no memory of enrolled addresses: that memory
  defended only the upgrade's one password in clear, and left with it; the account key's database
  drops the store an older console kept, with the addresses in it, the first time a page of this
  release opens it. A person from before the key scheme who never enrolled is told what a wrong
  password is told. One still signed in from before it (no `user.public_key`) is told in their
  account that their password no longer signs them in and to ask the administrator for a reset
  link, and is offered neither a password change nor a recovery code, nor asked to sign in
  again, which would not work.

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
says the person is, and answers its page with the `Session` that comes back (or the enrolment
ticket, below), exactly as `POST /v1/auth/login` answers the console; the console adopts it the
same way (`adoptSession`, [Editions](#editions)). No route of the core calls it. What an extension
calls, all of it in `internal/service/external.go`: `SignInExternal`, `EnrolExternal`,
`PinIdentityKey`, `MarkExternalStepUp` and `ExternalStepUp`. Each refusal is one of the fixed codes,
and the extension tells apart the ones its page acts on by their cause (`errors.Is`):
`auth.ErrEmailTaken` (an address somebody here has), `auth.ErrAccountKeyNeeded`,
`auth.ErrNoPlatformWrap` and `auth.ErrProductKeyChanged` (below).

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
  keeps theirs, whatever the provider now calls them. All of this, and the session or the
  enrolment ticket (below), is one transaction.
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
- **No password.** Such a person's `password_hash` is empty, they have not enrolled in the key
  scheme's password, and `password_changed_at` is 0. No auth key signs them in (it is checked
  against a dummy verifier, as an unknown address is), and `user.has_password` is `false`. A reset
  invitation (`user password --bootstrap`) gives them one, and a new account key: it deletes every
  platform wrap of the old one.
- **The account key** ([`key-scheme.md`](key-scheme.md) sections 6, 12.8 and 12.10). A provider may
  deliver a product key to the person's page alone, whose public half the extension pins
  (`PinIdentityKey`); the page keeps the person's account key wrapped under it, a platform wrap the
  server stores (`platform_wraps`) and never opens. Only a person who has an account key is given a
  session:
  - `ExternalSignIn.WantsKey` says the page asked for the product key, and `ProductKeyID` names it
    (`mailie:<epoch>`, pinned for the identity). Answered `ExternalSignedIn`: the session and, with
    `WantsKey`, `platform_wrap` (base64url, the person's wrap at that id), for the page to open;
  - a person without an account key gets no session. With `WantsKey` they are answered
    `enrolment` (`{ticket, seal_id, expires_at}`: single use, ten minutes, bound to the person, the
    identity and the product key id), and their page makes the account key and its wrap and sends
    them with the ticket, which the extension passes to `Service.EnrolExternal({ticket, public_key,
    platform_wrap, product_key_id})`: the public key is written once, the wrap stored insert only,
    and the session the sign-in would have opened (its length and its step-up time) is answered.
    Without `WantsKey` the sign-in is a `conflict` caused by `auth.ErrAccountKeyNeeded`, which
    created and linked nothing: the page signs in again asking for the key. So a session never
    exists for such a person, and nobody who copies one chooses their account key;
  - a person who has an account key and no wrap at the pinned id (one a reset invitation made) is
    a `conflict` caused by `auth.ErrNoPlatformWrap` when the sign-in asks for the key, with no
    session; for the identity alone they sign in as before.
- **The step-up time** of a session an extension starts is the provider's authentication time
  (`ExternalSignIn.AuthTime`, OpenID Connect's `auth_time`), at most now, never the moment of the
  sign-in: a sign-in the provider answered from its own session opens no step-up window. The
  extension's step-up is `Service.MarkExternalStepUp` (a mark on the session, used once, valid ten
  minutes) and then `Service.ExternalStepUp` with what the provider says (`ExternalStepUpProof`:
  the issuer, subject and authentication time, and the product key it names for the identity now),
  which refuses any identity but the one linked to the session's person and any time not after the
  mark, then compares the product key with the one pinned under its id, read only: another key, or
  an id nothing is pinned under, is a `conflict` caused by `auth.ErrProductKeyChanged`, logged as an
  error, and a step-up never pins. A provider that delivers no product key names none, which only an
  identity with nothing pinned may do. It answers the session's new step-up time.
- **The session's lifetime** is the extension's to choose: more than nothing and at most 14 days
  (`auth.SessionTTL`), absolute from its start. Nothing renews a session on use, and no statement
  can move an existing session's expiry later: the schema refuses an update that would
  (`sessions_expiry_fixed`, an upsert's included) and an insert under an id or token a session
  already has (`sessions_started_once`), which is how `INSERT OR REPLACE` would get around the
  first. The code only ever starts a session new, under a new id and token.
- **A key outlives the session it was created in.** A key an owner or an admin creates in a
  workspace while signed in (`POST /v1/workspaces/{id}/apikeys`) lasts what they chose, 30, 90 or
  365 days, whatever their session's lifetime: a short session bounds how long a browser stays
  signed in, not what the person deliberately hands a tool, which is what a key is for. So a session
  kept short because the provider cannot yet tell this server that it closed or locked someone does
  not close that gap for keys: until it can, the operator disabling the person here
  (`user disable`) is what revokes the keys they created and ends their sessions, in one
  transaction.
- **Key pins.** `Service.PinIdentityKey(issuer, subject, keyID, key)` keeps the public keys an
  identity is known by (`identity_key_pins`, at most 4096 bytes each): inserted the first time a key
  id is seen, then read back, in one transaction, so of two sign-ins racing each other both get the
  key that won. A pin is **never replaced**: an update is refused, and an insert under a key id
  already pinned changes nothing, whatever its conflict clause. It is **deleted only with the person
  it identifies**: the schema refuses to delete a pin while its identity is linked, and deleting the
  person (`user delete`, the second step of closing an account) removes their identities and then
  their pins in the same transaction. Disabling the person, the first step, keeps both, as it keeps
  their password: a person switched back on would sign in as before, and the pin must still hold
  then. No route or command switches a person back on yet (only `auth.Users.SetDisabled`, which
  tests use).
  An extension pins a key before the sign-in that links its identity; when that sign-in is
  refused (an address not verified, an address somebody here has already), the pin signs nobody
  in, and the hourly retention sweep deletes it once it is ten minutes old
  (`auth.UnlinkedPinGrace`), so it is kept a little over an hour at most.
- **`ExternalSignInOnly`** (`app.Options`, set only by a binary that embeds the daemon; there is no
  variable, and `serve` never sets it) is a server whose people sign in only that way. The service
  then refuses, `not_authorized`, every route that signs in with a password, signs up or accepts an
  invitation, changes a password, or creates an invitation to the instance or into a team:
  `POST /v1/auth/challenge`, `/v1/auth/login`, `/v1/auth/signup/open`, `/v1/auth/signup`,
  `/v1/auth/reset/open`, `/v1/auth/reset`,
  `/v1/auth/invites/accept`, `/v1/auth/password/begin` and `/finish`, `/v1/auth/recover/open` and
  `/finish`, `/v1/auth/recovery`, `/v1/auth/stepup`, `/v1/users/invites` and
  `/v1/workspaces/{id}/invites`. No salt key is made there. The command line's `--bootstrap`
  commands still write to the database, but an invitation they print signs nobody up there. Left at
  its zero value nothing changes.

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
- **Workspace keys**, which an owner or an admin of a workspace creates in the console (see
  [API keys](#api-keys) below), scope `read`, `write` or `send`. They act as nobody and reach the
  mailboxes of their workspace they hold something on; they are the only way a tool reaches a
  mailbox outside the operator workspace.

A session counts as a person with every scope; what it may do to a mailbox still depends on
their grant on it and their consent.

### Rate limits

Sign-in and sign-up: 60 a minute per address (burst 20) and 5 a minute per email address, before
any hashing. That second budget is the account's, whichever route spends it: a recovery, a reset
and a session's step-up, password change and recovery code replacement (keyed by the person's
stored address) all draw on the same 5. Authenticated requests spend from a bucket
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
- **On a mailbox that has a key**, reading takes the `read` flag and the person's **grant** at its
  current epoch ([Mailbox keys](#mailbox-keys)); a member who holds the flag without it waits for
  the key, sees the card and reads nothing, and is no reader.
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
mailbox someone reads keeps a **reader** — the last person who can read it (the flag, and on a
mailbox that has a key a grant at its current epoch) keeps `read`, and is not disabled, removed,
reset, or closed without `force`. A team mailbox nobody can read (a forced closure)
syncs nothing, is never switched on again (`409`), and is marked `no_reader`; its owners and admins
remove it, or remove it and link it again, or turn its sync off to delete what is still indexed. A team mailbox syncs under its **workspace's consent** ([Sync and consent](#sync-and-consent)).

### Workspaces in the console

The console decides nothing here: every rule is the server's, and a screen only offers what the
caller's role and flags allow and says beforehand what a protection refuses (`web/src/ui/access.ts`,
whose rules are tests of their own). A refusal is said in the console's words, from its `code`.

- **The workspace shown.** The sidebar (the drawer on a phone) has a switcher once the person
  belongs to more than one workspace, and the header's breadcrumb names the one shown on the
  sections that are a workspace's (Mailboxes, Members, API keys & MCP, Storage), never on the
  person's own (Account), which an edition may mark `scope: 'person'` (its account section always
  is).
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
- **API keys are the workspace's** ([`workspaces.md`](workspaces.md), "API keys"). **API keys &
  MCP** is there for an owner or an admin of the team shown, or the person of their personal
  workspace, and not for a member, whose team says that its owners and admins manage its people,
  access and API keys (the console never asks a member's team for its keys). It lists every key of
  the workspace, revoked and expired ones too, live first: what each holds on each mailbox, who
  created it (in a team), and what a key from before keys belonged to workspaces does differently
  (`origin`, `carried_over`). Creating one asks for a name, a scope (`send` only where
  `GET /v1/me/mcp` says `keys_send`), what it holds on each of the workspace's mailboxes
  (`GET /v1/accounts?workspace=`: `read` only on those the viewer reads, `act` with `read` and a
  scope that acts, `send` with the send scope; none, a key that reaches nothing yet) and a
  lifetime, above the edition's key terms, worded for the team or the personal workspace; the
  secret is shown once. A key's sheet changes what it holds, mailbox by mailbox, ticked and then
  saved (`PUT …/accounts/{account_id}` with all three flags, or `DELETE` when none is left), gives
  a carried-over key nothing, and lists its sends (`GET …/sends`: state, mailbox, time and how many
  recipients, never who or what) for a key that can send. Revoking asks first; for a carried-over
  key it takes this workspace's mailboxes out of it, and says so after (the key may still work
  elsewhere). The workspace's limit of 20 counts what the server counts: neither carried-over keys
  nor the ones the upgrade moved in (`origin`). Each team mailbox's **Access** lists the keys
  holding something on it (`keys`), never as readers, and takes the mailbox out of one after
  asking. **Account** lists the keys the person created, in every workspace (`GET /v1/me/apikeys`),
  once there is one, and revokes any of them (`DELETE /v1/me/apikeys/{prefix}`), a carried-over one
  in every workspace, which its note there says.
- **The person's own settings are not a workspace's.** The dialog
  that turns the person's own sync off says it deletes the index of
  their personal mailboxes, and, to someone in a team, that a team's mailboxes sync under the
  team's consent and keep syncing, but for one they linked before the upgrade whose carried-over
  consent no owner or admin has confirmed yet, which stops with its index. It reads no list first.
- **Each mailbox** says on its card what the person may do with it (`access`: Read, Act, Send,
  Manage). Folders and Sync need `read`; authorizing again needs manage; a mailbox seen without
  `read` (an owner's or an admin's card of a team mailbox, say) says so instead of offering them,
  and one that needs authorizing tells whoever does not manage it that an owner or an admin has
  to. One whose key the person waits for (`access.waiting_key`) says that instead, apart from not
  reading it: in a team, that anyone who reads it can hand the key over; a personal one, that its
  details offer a new key ([Mailbox keys](#mailbox-keys)). Removing is a team's owners' and admins' (or a personal mailbox's person's): it asks for the
  mailbox's address typed and sends its id as `confirm`.
- **A member of a team** gets the switcher and cards for the mailboxes they hold a grant on
  (re-authorizing where they hold `manage`; an edition with Mail and Compose offers those they read
  or may send from). No Members, no API keys, no Access, no team sync switch and no removing: one
  line instead, on the team's mailboxes and in Members, says that its owners and admins manage the
  team's people, who can use each mailbox and its API keys; the console never asks for the members,
  the directory or the keys, which the server refuses them. No Leave. Nor is the person's own
  consent card asked in a team: a team's mailboxes sync under the team's consent, and a member's
  sheet says that its owners and admins turn it on.
- **Who can use a mailbox** (the sheet's Access, in a team, for its owners and admins): every
  active member with their flags, ticked and then saved. `read` is offered only to a viewer who
  reads the mailbox (on one that has a key, the flag and the key: `readsNow`, the directory's
  `sealed` at its `epoch`); `act` only for someone who reads; `send` for anyone; `manage` for
  members (owners and admins manage by their role, so theirs is shown ticked and is not theirs to
  change). Someone who holds Read without the key is marked as waiting for it. Read given on a
  mailbox that has a key to a member with an account key (the members list's `seal_id` and
  `public_key`) goes with their grant, sealed in the viewer's browser from their own after a
  step-up, which the row says before it is saved; to a member without one, and on a mailbox
  without a key, by the flag alone.
  The mailbox's last reader (`last_reader_of`, or `readers` of 1) is marked, and taking their read
  is not offered; an owner or an admin who does not read it is told that they cannot give Read,
  not even to themselves, and one who waits for its key, that they can once someone who reads it
  hands it over; a mailbox nobody can read
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
  belongs to; a provider's return shows the workspace of the mailbox it authorized. A link carries
  the mailbox's first key, and asks for the step-up first when the session's has less than two
  minutes left ([Mailbox keys](#mailbox-keys)).
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
| `POST /v1/auth/challenge` | anyone | `{email}` → `{salt, kdf}`; the release that brought the key scheme also answered `upgrade`, removed since |
| `POST /v1/auth/login` | anyone | `{email, auth_key}` → `Session` with `password_wrap`, and `rederive {salt, kdf, ticket}` when the account is off its target |
| `POST /v1/auth/signup/open` | anyone | `{invite, email}` → `{salt, kdf, seal_id}`; checks the invitation as the sign-up will |
| `POST /v1/auth/signup` | anyone | `{invite, email, name, seal_id, auth_key, kdf, public_key, password_wrap, recovery_wrap, recovery_proof}` → `Session` (201); an instance invite, or a team invite the operator or an instance owner made |
| `POST /v1/auth/reset/open` | anyone | `{reset, email}` → `{salt, kdf, seal_id}`, the account's target, which the reset stores, and the person's seal id; nothing changes |
| `POST /v1/auth/reset` | anyone | `{reset, email, auth_key, kdf, public_key, password_wrap, recovery_wrap, recovery_proof}` → `Session`; a new account key, every session and grant of the old one ends |
| `POST /v1/auth/recover/open` | anyone | `{email, recovery_proof}` → `{seal_id, public_key, recovery_wrap, salt, kdf, ticket}` |
| `POST /v1/auth/recover/finish` | anyone | `{ticket, current_recovery_proof, auth_key, kdf, password_wrap, recovery_wrap, recovery_proof}` → 204; every session ends; `current_recovery_proof` is the proof that opened the recovery (`recover/open`'s), `recovery_proof` the new code's |
| `GET /v1/auth/me` | session | `{user, session}`; `user.has_password` is `false` for a person who signs in only through an extension; `user.seal_id`, `user.public_key`, `session.authenticated_at` |
| `POST /v1/auth/logout` | session | `{everywhere?}` → 204 |
| `POST /v1/auth/password/begin` | session | `{current_auth_key}` → `{password_wrap, salt, kdf, ticket}` |
| `POST /v1/auth/password/finish` | session | `{ticket, current_auth_key, auth_key, kdf, password_wrap}` → a new `Session` (a change: every other session ends), or 204 (a sign-in's re-derivation); `current_auth_key` is the auth key that earned the ticket (`password/begin`'s, or the sign-in's) |
| `POST /v1/auth/recovery` | session, current auth key | `{current_auth_key, recovery_wrap, recovery_proof}` → 204 |
| `POST /v1/auth/stepup` | session | `{auth_key}` → `{authenticated_at}` |
| `PUT /v1/auth/profile` | session | `{name}` → `User` |
| `POST /v1/users/invites` | instance owner signed in, or unrestricted instance admin key | `{email, role?}` → `Invite` |
| `POST /v1/auth/invites/accept` | session | `{invite}` → `Workspace`: joins the team with the invite's role |
| `POST /v1/users/disable` | owner signed in (never on themselves), or unrestricted instance admin key | `{email, force?}`: sessions ended, keys revoked, and `team_syncs_stopped` |
| `POST /v1/users/delete` | owner signed in (never on themselves), or unrestricted instance admin key | `{email, force?}` → what was deleted, and `team_syncs_stopped` |
| `GET /v1/providers` | read | which providers and flows **this** caller can use, the default first |
| `GET /v1/workspaces` | read | the caller's workspaces with their role in each; an instance key the operator workspace; the operator every workspace, with counts |
| `POST /v1/workspaces` | session; operator | `{name}` (the operator adds `owner_email`) → `Workspace` (201), a team |
| `PATCH /v1/workspaces/{id}` | team owner or admin; operator | `{name}` → `Workspace` |
| `GET /v1/workspaces/{id}/members` | owner or admin; operator (a member: `403`) | `[Member]`, with `last_owner`, `last_reader_of`, and each person's `seal_id` and `public_key` (absent until they enrol), which a grant to them is bound by and sealed to |
| `PATCH /v1/workspaces/{id}/members/{user}` | owner: anyone's; admin: members', never to admin or owner; operator | `{role?, status?}` → `Member`; any change expires the invites the person made there |
| `DELETE /v1/workspaces/{id}/members/{user}` | owner: anyone, themselves while another owner remains; admin: members; operator | 204; their grants there go, their invites there expire |
| `GET/POST /v1/workspaces/{id}/invites`, `DELETE …/invites/{invite}` | owner; admin (member invites); operator | team invites: `{email, role?}` → `TeamInvite` with its `url` (201) |
| `GET /v1/workspaces/{id}/access` | owner or admin; operator (a member: `403`) | `[MailboxAccess]`: every mailbox, its grants (each with `sealed`: its person holds the key at the current epoch), `readers` (by the one rule), `no_reader`, `epoch` (its key's current one; absent without a key), its own consent to sync (`sync`) and `linked_by`; never the index |
| `PUT /v1/accounts/{id}/access/{user}` | owner or admin (see [Workspaces](#workspaces)); operator: `manage` to members only | `{read, act, send, manage, grant?, public_key?}`, all four flags → `Grant`; `grant` with `read` given on a mailbox that has a key to a person with an account key, after a fresh step-up when they are someone else, and `public_key`, the account key it was sealed to, with it ([Mailbox keys](#mailbox-keys)) |
| `DELETE /v1/accounts/{id}/access/{user}?flags=` | owner or admin, their own flags included; operator | 204; `flags` (`read,act,send,manage`) names what goes, every flag without it; taking `read` takes the person's grants |
| `GET /v1/accounts/{id}/mailbox-key` | session holding `read` on it | `MailboxKeyState` ([Mailbox keys](#mailbox-keys)) |
| `POST /v1/accounts/{id}/mailbox-key` | session reading it, with an account key; fresh step-up | `{public_key, namespace, grants: [{user_id, grant, public_key}]}` → `MailboxKeyPair` (201): the first key of a mailbox without one |
| `PUT /v1/accounts/{id}/mailbox-key` | session, a personal mailbox's person; fresh step-up | `{epoch, public_key, grant}` → `MailboxKeyPair`: its next key |
| `PUT /v1/accounts/{id}/grants/{user}` | session reading it; fresh step-up | `{epoch, grant, public_key}` → `SealedGrant`: the key, to a member who holds `read` without it |
| `GET /v1/accounts`, `GET /v1/accounts/{id}` | read | the mailboxes the caller holds a grant on, and every mailbox of a team they own or administer (its card); `?workspace=` narrows the list |
| `POST /v1/accounts` | admin (a session counts) | add a mailbox (45 s); `workspace_id` names a team the caller owns or administers, and `sync_consent_version` (the current sync text) gives the team's consent with the link; a person sends the mailbox's first key, `public_key`, `namespace` and `grant`, with a fresh step-up |
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
| `GET /v1/me/mcp` | read | `{http, keys_send}`: whether this server answers MCP over HTTP at `/mcp`, and whether its API keys may send (`MAIL_KEYS_MAY_SEND`) |
| `GET /v1/me/apikeys`, `DELETE /v1/me/apikeys/{prefix}` | session | the keys the person created, in every workspace; `POST` answers `400` |
| `GET/POST /v1/workspaces/{id}/apikeys`, `DELETE …/apikeys/{prefix}` | session, owner or admin | the workspace's keys |
| `PUT/DELETE /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account_id}` | session, owner or admin | what a key holds on a mailbox |
| `GET /v1/workspaces/{id}/apikeys/{prefix}/sends` | session, owner or admin | the key's sends |
| `GET /v1/events` | read | Server-Sent Events of the mailboxes the caller may read; `?workspace=` narrows |
| `GET /v1/events/wait` | read | long poll for new mail; `?workspace=` narrows |
| `GET /v1/messages` | read | search the index; `?workspace=` narrows |
| `GET /v1/messages/{id}` | read | a message, its body fetched from the mail server now |
| `GET /v1/messages/{id}/raw` | read | the original (`message/rfc822`), always as an attachment |
| `GET /v1/messages/{id}/attachments/{path}` | read | one part, by its IMAP section, always as an attachment |
| `PATCH /v1/messages/{id}`, `POST /v1/messages/{flags,move,trash}` | write | actions |
| `POST /v1/messages/send` | send | send a message |
| `GET /v1/sends/{key}?account=` | send | the record of one of the caller's own sends |
| `GET/POST /v1/apikeys`, `DELETE /v1/apikeys/{prefix}` | admin, only with `MAIL_ADMIN_API=true` | every key, with its `workspace_id`; issues instance keys; `mailserver apikey create\|list\|revoke` without `--bootstrap` call these |

`/v1/users/disable` and `/v1/users/delete` carry the address in the body, never in the URL. A
server whose people sign in only through an extension answers `403 not_authorized` to the routes
that sign in with a password, sign up or accept an invitation, change a password or create an
invitation ([Signing in through an extension](#signing-in-through-an-extension)).

There is no current workspace on the server: a session reaches the person's mailboxes in every
workspace they belong to (a key, only what it holds in its own), and every account (and every storage entry) carries its
`workspace_id`. The console keeps the workspace it shows itself — per tab, remembered for each
person in local storage — and passes `?workspace=ID` to `GET /v1/accounts`, `GET /v1/messages`,
`GET /v1/me/storage`, `GET /v1/events` and `GET /v1/events/wait`, which narrows them to that
workspace's mailboxes, so a view never fetches another workspace's data; a workspace the caller is
not an active member of is `404`, and with `account` as well the account must be in it
(`docs/workspaces.md`, "Choosing the workspace in a request").

An account carries `workspace_id`, `linked_by` (absent for the operator's mailboxes) and `access`,
the caller's own flags as far as their credential's scope reaches (`read` is reading now, by the
one rule); `send.reason` `not_granted` is a mailbox the caller may not send from. For a person
signed in it also carries `mailbox_key`, `{epoch, public_key, namespace}`, the mailbox's key pair at
its current epoch (absent for a mailbox without a key), and `access.waiting_key: true` when they
hold `read` on a mailbox that has a key without a grant at its current epoch. An API key sees
neither, over REST or MCP.

## Mailboxes

### Adding one

`POST /v1/accounts` takes `{email, provider?, display_name?, password?, imap_host?, imap_port?,
smtp_host?, smtp_port?, smtp_tls?, login_user?, flow?, initial_days?, save_sent_copy?,
workspace_id?, sync_consent_version?, public_key?, namespace?, grant?}` and answers the account
and, for OAuth, the authorization to complete. A person's link carries the mailbox's first key
([Mailbox keys](#mailbox-keys)), written in the transaction that creates the mailbox, on the
password and the OAuth paths alike; resuming an abandoned link (`POST /v1/accounts/{id}/oauth/start`)
carries none, since the row has it. A password account is **tested before it
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

### Mailbox keys

Phase 3 of the key scheme ([`key-scheme.md`](key-scheme.md) sections 8, 9 and 12.11 to 12.15;
[`workspaces.md`](workspaces.md), "Mailbox keys and sealed grants"). A mailbox's key pair is made
by a browser: an X25519 key pair, a **namespace** (a lowercase UUIDv4, the same at every epoch, no
other mailbox's) and an **epoch** (1, then each new key the next). The server stores the public half
and the namespace, and **grants**, the private key sealed to one person's account public key, 88
bytes of base64url (118 characters) it checks for their shape only and cannot open; no route
takes a private key, and the request decoder refuses an unknown field. Every route here is a
person's: an API key is `403`.

- **Who reads.** On a mailbox that has a key, the `read` flag and a grant at its current epoch; on
  one without, the flag alone. A member who holds the flag without the grant **waits for the key**
  (`access.waiting_key`), and reads nothing of it.
- **Linking** (`POST /v1/accounts`, section 12.11): `public_key` (base64url of 32 bytes),
  `namespace` and `grant` (the linker's own, at epoch 1), all three, with a fresh step-up, asked
  before the mail server is dialled and again in the transaction that stores the mailbox. A
  person without an account key cannot link (`409`); an instance key's link carries none (`400`).
- **Giving read** (`PUT /v1/accounts/{id}/access/{user}` with `grant`, section 12.13): the
  recipient's grant at the current epoch, sealed by the giver's browser from its own; required
  when `read` is added on a mailbox that has a key to a person with an account key, refused
  otherwise. A fresh step-up when the recipient is someone else.
- **Supplying the key** (`PUT /v1/accounts/{id}/grants/{user}` `{epoch, grant, public_key}` →
  `SealedGrant`): any person who reads the mailbox, owner, admin or member, to an active member
  who holds `read`, has an account key and no grant at the current epoch; a fresh step-up.
- **The first key** (`POST /v1/accounts/{id}/mailbox-key` `{public_key, namespace, grants: [{user_id,
  grant, public_key}]}` → `MailboxKeyPair`, 201, section 12.14): a mailbox without a key, from a
  person who reads it by the flag and has an account key, with exactly one grant at epoch 1 for
  them and for every other active member who holds `read` and has an account key
  (`keyless_readers` below); a fresh step-up. The console writes it right after a sign-in, while
  that sign-in counts as a step-up.
- **The key a grant was sealed to.** Every grant posted for someone names the account public key
  it was sealed to: `public_key` (base64url, 32 bytes) beside `grant` when giving read and when
  supplying the key, and in each of a first key's `grants`, the writer's own included. The server
  refuses the grant (`409`, nothing written) unless that is the recipient's account key now: a
  console that read them before their reset (a new account key, the same seal id) would otherwise
  seal them a grant that counts them a reader, keeps the key from being supplied to them, and
  never opens. The server still cannot check what the bytes were sealed to; it holds the stated
  key to the stored one ([`key-scheme.md`](key-scheme.md) section 9.3 and Appendix C). A
  `public_key` without a grant is `400`.
- **A new key** (`PUT /v1/accounts/{id}/mailbox-key` `{epoch, public_key, grant}` →
  `MailboxKeyPair`, section 12.12): a personal mailbox's person only, at the epoch after the
  current one, with their own grant, keeping the namespace; every grant of an older epoch goes. A
  team mailbox is never given one (`403`). After a reset this is how a person reads their own
  mailboxes again.
- **What the console reads**: each account's `mailbox_key` and `access.waiting_key` (above), and
  `GET /v1/accounts/{id}/mailbox-key`, for a person who holds `read` on it (one who sees only its
  card: `403`), which answers

  ```jsonc
  {"epoch": 1, "public_key": "…", "namespace": "9d035f2b-…",  // absent for a mailbox without a key
   "grant": "TUwBAQEAAQ…",      // the caller's own at the current epoch; absent while they wait
   "waiting": [{"user_id": "usr_…", "email": "…", "name": "…", "seal_id": "…", "public_key": "…"}],
   "suppliers": [{"user_id": "usr_…", "email": "…", "name": "…"}],
   "keyless_readers": [{"user_id": "usr_…", "email": "…", "name": "…", "seal_id": "…", "public_key": "…"}]}
  ```

  `waiting`, for a reader, are the members to supply; `suppliers`, for a member who waits, who
  reads it now; `keyless_readers`, on a mailbox without a key, whom its first key is sealed to
  beside its writer. The members list (`GET /v1/workspaces/{id}/members`) carries each person's
  `seal_id` and `public_key` for an owner or an admin giving read.
- **Taking read** — a revoke, a membership disabled or removed, the person disabled, deleted or
  reset — deletes their grants in the same transaction.
- **Events.** Every write of a key or a grant has open streams check access again, and `event:
  access` reaches only the person whose reading the write changed: `"read": true` when the key is
  supplied to them, or when they write a new key for their own mailbox while they waited for it;
  `"read": false` when a first key is written with no grant of theirs (they have no account key
  yet). A write that changes nobody's reading sends nothing: a supply tells the other readers
  nothing, though their `waiting` changed, nor does a first key tell its writer or those it is
  sealed to, though `keyless_readers` emptied. A console reads `GET /v1/accounts/{id}/mailbox-key`
  again after its own writes rather than wait for an event, and again when a write answers `409`:
  someone else's write came first.

| Situation | Code |
|---|---|
| a public key, namespace, grant or epoch outside its shape; a grant at odds with the epoch its request names; a grant without the `public_key` it was sealed to, or a `public_key` without a grant; a person's link without its key, an instance key's with one; `read` on a mailbox that has a key, to a person with an account key, without their grant; a grant with a change that gives no `read` | `400` |
| a step-up more than ten minutes old, or none; a giver or a writer who does not read the mailbox now; a person who sees the mailbox without holding `read` asking for its key; a team mailbox's new key; an API key | `403` |
| a mailbox the caller cannot see; a recipient who is not an active member of its workspace | `404` |
| a grant at another epoch than the current one, or a new key at another than the next; a grant that already exists; a grant that names another account key than its recipient's now; a first key for a mailbox that has one, or whose grants are not exactly its readers' with an account key; a namespace in use; a recipient without an account key, or without `read` for the key; a person without an account key linking | `409` |

#### The console's half

The browser makes every mailbox key and seals every grant; the server never receives a private
one ([`key-scheme.md`](key-scheme.md) section 16):

- **The cryptography** is `web/src/crypto/mailbox.ts`, over the kit's `hpke` and Mailie profile: a
  key pair (`generateKeyPair`) and a namespace (`newNamespace`) at epoch 1, or a personal mailbox's
  own namespace at its next epoch; a grant sealed (`sealGrant`) to the public key and seal id the
  server serves, both checked first; and the person's own grant opened (`openGrant`) against the
  public key the server holds for the mailbox at that epoch. Every mailbox private key is zeroed as
  soon as its grants are sealed, and one is open at a time. A seal id or a key outside its
  spelling, a recipient key of low order, a grant that does not open, and one that opens to
  another key than the mailbox's are all `security`: nothing is sealed or sent.
- **The account key that opens grants** comes from the browser vault as the kit's
  non-extractable private key (`accountPrivateKeyOf` in `web/src/state/accountVault.ts`, under the
  vault's rules: the person `GET /v1/auth/me` names, a record of anyone else wiped). Three paths
  read the raw key back out (`accountKeyOf`), each zeroing it at once: a session restored on a
  page load, to set `session.keyed` (`holdsAccountKey`); a password change, which checks the key
  kept here against the server's wrap; and a new recovery code, which wraps it again. A browser
  without it (`session.keyed` false: a sign-in elsewhere, storage refused) says `no_account_key`,
  to sign in again there, before the step-up is asked and before anything is sent: giving Read
  with a grant and handing the key over check it first, and the access panel says so beside a
  Read that would carry the key. Sealing to someone else needs no account private key.
- **Linking** (`state/accounts.ts`): a fresh pair and namespace on every attempt, the linker's
  grant at epoch 1 sealed to their own `user.public_key` under their `seal_id`, sent as
  `public_key`, `namespace` and `grant` in the `POST /v1/accounts` that creates the mailbox, by the
  password form and the OAuth flows alike, and the private key zeroed before the page leaves for the
  provider. A person without an account key (signed up before the key scheme, never enrolled, and
  still signed in from then) is told to ask the administrator for a reset link (`not_enrolled`)
  before anything is asked or sent. Finishing an abandoned link sends no
  key: the mailbox has its own.
- **Giving Read** (`state/team.ts` `saveGrant`): Read added on a mailbox that has a key (the
  directory's `epoch`) to a member with an account key reads the member again
  (`GET /v1/workspaces/{id}/members`) and the key (`GET …/mailbox-key`), opens the giver's own
  grant, seals the key to the member as that answer lists them, at the current epoch, and sends it
  as `grant`, with the `public_key` it was sealed to, beside the four flags, after a step-up;
  otherwise the flags alone, as before, with no step-up. Whether a grant goes is decided from the
  directory and the members as the page read them, which nothing says are stale (another reader
  may have written the mailbox's first key, the member enrolled, been reset or been given Read
  since): a change that gives Read and is refused `400` or `409` reads both again, and the
  mailbox's card, and is made once more when what it would send differs (with a grant now, without
  one, or sealed to another key); a member given Read meanwhile is said to be (`give-read`,
  `conflict`), and a refusal that nothing read again explains is said for what it is. A browser
  without the giver's account key says so beside the row, and before the step-up is asked.
- **The key's section of a mailbox's sheet** (`components/MailboxKeyPanel.vue`,
  `state/mailboxKeys.ts`), for anyone who holds Read on it, owner, admin or member: a member who
  waits for the key is told who can hand it over (`suppliers`), or that nobody reads the mailbox
  now; someone who reads it is shown who waits for it (`waiting`) and hands it to each, sealed from
  their own grant to the member as the key read in the same call lists them, never as the sheet
  showed them, and once more after a `409` (`PUT …/grants/{user}`); a mailbox read without a key
  offers its first key,
  naming whom it is sealed to beside them (`keyless_readers`, `POST …/mailbox-key`); and a
  personal mailbox whose person waits for its key, or whose own grant does not open in this
  browser (tried when its sheet opens), offers a new key at the next epoch, sealed to them alone
  (`PUT …/mailbox-key`). A team mailbox is never offered a new key: one nobody reads keeps its
  "nobody can read" note, and is only removed. The section reads the key, and tries the person's
  own grant, when the sheet opens and when the card says something new of it (another epoch, Read
  or the key gained or lost), never because a sync replaced the card. Each write asks for the
  step-up first, and reads the key, the card and the team's directory again after it, or after a
  `409`.
- **First keys after a sign-in**: right after every sign-in in this page (`state/session.ts`
  `onSignIn`, from `beginSession`), never on the page load of an older session, while the
  sign-in's step-up time counts and this browser keeps the person's account key, the console reads
  `GET /v1/accounts` and writes the first key of every mailbox the person reads without one (never
  an operator's), one request per mailbox, sealed to them and to its `keyless_readers`; a `409` (someone enrolled, or
  another tab keyed it) reads the mailbox key again and tries once more. A mailbox it did not key
  is left for its sheet.
- **Failures** are said by operation (`give-read`, `load-mailbox-key`, `supply-key`, `first-key`,
  `new-key`, and `not_enrolled` for a link), in the console's words, never the server's.

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
| `MAIL_CONSENT_VERSION_ACTIONS` | `2026-10-open-actions-3` | the server changing a mailbox when someone allowed to act on it asks, and a key given Act acting under the key terms, not under this agreement (`web/src/open/ActionsText.vue`) |
| `MAIL_CONSENT_VERSION_SEND` | `2026-10-open-sending` | sending from their mailboxes (the open console has no such text) |
| `MAIL_CONSENT_VERSION_KEYS` | `2026-10-open-api-keys-2` | what a tool holding a new key can do (`web/src/open/KeyTermsText.vue`) |

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

`MAIL_KEYS_MAY_SEND` goes with the key terms: `false` is for a console whose key terms do not say a
key may send. The `send` scope and the `send` flag are then refused when a key is created or given
a mailbox, every send by a workspace key is refused, and `sends` is `false` on every key, and
`GET /v1/me/mcp` says `keys_send: false`, so a console offers neither. It defaults to `true` only
under the open console's key terms; a server whose `MAIL_CONSENT_VERSION_KEYS` names other terms
must set it, or the daemon refuses to start, since only those terms say whether a key may send.
Those terms are also what a key's actions answer to: a key acts where it is given Act whatever
anyone chooses about actions in their account, but for a person's key the upgrade to workspace keys
carried over, which acts only while its creator allows actions.

`MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT` goes with them too: `true` is for a console whose key terms
say a key acts only while its person allows actions. Every workspace key then acts only while the
person who created it is active and agrees to the actions text at the current revision, as a
person's key carried over by migration 0012 always does. It defaults to `false` under the open
console's key terms; a server whose `MAIL_CONSENT_VERSION_KEYS` names other terms sets it, or the
daemon refuses to start.

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
`MAIL_CONSENT_VERSION_ACTIONS`. `GET /v1/me/actions-consent` reads it (a session; no key, which
acts as nobody). `POST {"version": …}` and `DELETE` are the person's, signed in. Withdrawing
deletes nothing and stops actions at once: every action checks the consent before connecting and
again, on the connection, before each command that changes the mailbox. `ActionsConsent` has the
shape of `SyncConsent`. Without sync there is no index, and without an index no message to act on;
an index that stays while sync is stopped is not acted on either (point 6 below).

### Who may act

Decided in `internal/service`, before any connection, in this order:

1. The `write` scope; a `read` key gets `403`.
2. Every message must be readable by the caller (`read` on its mailbox); one they cannot read, or
   one that does not exist, makes the whole request `404`. All in one account, 1 to 100 ids.
3. **A person's or a team's mailbox**: a person signed in must hold `act` on it, and **their own**
   consent to actions must name the current revision — whoever linked the mailbox. Without `act`,
   `403`; without consent, or with an old one, `409`. A workspace key must hold `act` on it: the
   key terms its creator agreed to cover what it does, and no person's consent is asked (a
   person's key the upgrade carried over acts only while its creator allows actions, `409`
   otherwise). Asked again, the key still live, before each command.
4. **A mailbox of the operator workspace**: an instance key with `write`.
5. The account must be usable: `needs_reauth`, `pending_auth` and `disabled` are `409`.
6. The mailbox must be syncing, so that the index can follow what the server does. A team mailbox
   kept stopped since migration 0011 keeps its index for its readers, and one nobody reads any more
   keeps it for a key that holds it, but no row of it can change: an action there is `409` before
   the server is touched. Asked again, with the rest, on the connection before each command; when
   sync stops between that and the recording (an owner or an admin turning the team's sync off,
   say), the answer is `409` too, and says the server may have made the change.

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

1. The `send` scope (sessions have it; a workspace key only if created with it).
2. `confirm: true` in the message. Without it, `400` before anything else: nothing is reserved or
   dialed. No flag or test relaxes this.
3. The account must be visible to the caller (`404` otherwise).
4. **A person's or a team's mailbox**: a person signed in must hold `send` on it, with **their
   own** consent to sending at the current revision. Without `send`, `403`; without consent, `409`.
   A workspace key must hold `send` on it, on a server whose keys may send
   (`MAIL_KEYS_MAY_SEND`, `403` otherwise); no person's consent is asked, the key terms cover it,
   and the key is asked again, still live, before each connection. A reply or a forward also needs
   `read` on the original.
5. **A mailbox of the operator workspace**: an instance key with `send`.
6. The account must be usable (`409` otherwise).

Each account's JSON has `send: {available, reason?, from_name?}`: whether it can send **for this
caller**, consent aside. `reason` is `needs_reauth`, `pending_auth`, `disabled`, `no_smtp` or
`not_granted` (no `send` flag, or a credential that does not send). `from_name` is the caller's
own profile name: a message goes out under the name of whoever sends it.

### `POST /v1/messages/send`

`multipart/form-data`, 3 minutes: first a `compose` part (JSON), then zero or more `attachment`
parts, each a file. An `Idempotency-Key` header (1 to 128 letters, digits, `-`, `_`, `.` or `:`) is
**required for a session** and optional for a key; without it, a key's sends are keyed by a keyed
hash of the message and the key, and the minute (`<hash>/<minute>`), so the same key repeating the
same message within the minute is a replay, not a second send, while another key sending the same
message from the same mailbox sends its own.

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
most 200 messages a day, a workspace key 100, and each account at most its provider's rate per
minute (`429`). A key's message goes out under the address alone, and its record and
`send.finished` name the key (`sent_by`), which alone reads them.

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

A workspace's keys are its owners' and admins' (in a personal workspace, its person's), for a tool —
an assistant over MCP, a script over REST — to read the mailboxes they give it and, with `write`,
act on them, with `send`, send from them. Creating a key is its creator's agreement to the key
terms, which say what a tool holding it can do. Only an owner or an admin **signed in** lists,
creates and revokes the workspace's keys and changes what they hold: any key, the new one included,
gets `403` on these routes, and a member does too. No key creates a key.

| Route | Body | Answer |
|---|---|---|
| `GET /v1/workspaces/{id}/apikeys` | | `[WorkspaceKey]`, live first, newest first, revoked and expired ones included |
| `POST /v1/workspaces/{id}/apikeys` | `{name, scope, ttl_days?, terms_version, mailboxes?: [{account_id, read, act, send}]}` | `201` `WorkspaceKey` + `{key}` |
| `DELETE /v1/workspaces/{id}/apikeys/{prefix}` | | `204`, also when already revoked |
| `PUT /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account_id}` | `{read, act, send}`, all required | `KeyMailbox` |
| `DELETE /v1/workspaces/{id}/apikeys/{prefix}/accounts/{account_id}` | | `204` |
| `GET /v1/workspaces/{id}/apikeys/{prefix}/sends` | | `[SendStatus]`, newest first |
| `GET /v1/me/apikeys` | | `[WorkspaceKey]`: the keys the person created, in every workspace, with everything each holds |
| `DELETE /v1/me/apikeys/{prefix}` | | `204`: revokes a key the person created |
| `POST /v1/me/apikeys` | | `400`: keys are created in a workspace |

```jsonc
{
  "prefix": "0a0b0c01",            // names the key; not a secret
  "name": "Claude Code",
  "scope": "read",                 // "read" | "write" | "send"
  "workspace_id": "wsp_…",         // absent for a key carried over from before (carried_over)
  "mailboxes": [{"account_id": "acc_…", "workspace_id": "wsp_…", "read": true, "act": false,
                 "send": false, "granted_by": "usr_…", "updated_at": 1790000000}],
  "created_by": "usr_…",           // who created it; absent once they are deleted
  "created_at": 1790000000,
  "expires_at": 1797776000,
  "last_used_at": 1790003600,      // absent if never used; one-minute resolution
  "revoked_at": 1790000000,        // absent while alive
  "live": true,                    // neither revoked nor expired
  "terms_version": "2026-10-open-api-keys-2",
  "sends": false                   // can send at all: the send scope, on a server whose keys may send
  // "origin": "person" | "person-all" for a person's key the upgrade moved into its workspace;
  // "carried_over": true and "other_workspaces": n for one it carried over with no workspace
}
```

- `name`: 1 to 120 characters. `scope`: `read`, `write` or `send` (`send` is `400` where
  `MAIL_KEYS_MAY_SEND=false`). `ttl_days`: 30, 90 or 365 (default 90).
- `mailboxes`: each a mailbox of the workspace (another is `404`, and nothing is created), each
  flag allowed by the scope (`act` needs `write` or more and `read`, `send` needs `send`; `400`
  otherwise). `read` only where the person creating it **reads the mailbox themselves** (`403`
  otherwise); `act` and `send` any owner or admin gives. The same holds on `PUT …/accounts/…`:
  `read` the key does not hold yet only from someone who reads the mailbox; taking flags away
  needs nothing. A key with no mailbox reaches nothing, and is not revoked for that.
- What a key holds **stands on its own**: whoever gave it, and the person who created it, may lose
  their own access, and the key keeps its mailboxes until an owner or an admin takes them out or
  revokes it. The access directory (`GET /v1/workspaces/{id}/access`) lists each mailbox's live
  keys (`keys`); keys never count as readers.
- `terms_version` must be `MAIL_CONSENT_VERSION_KEYS`; another is `409`. A key keeps working under
  the terms it was created with.
- At most **20 live keys** per workspace; the 21st is `409`.
- The secret appears **once**, in the `POST` answer (`Cache-Control: no-store`).
- A key is revoked when the person who created it leaves the workspace, or is disabled or deleted
  on the instance; demoting them, or disabling their membership, keeps it.
- A key sends through `POST /v1/messages/send` from a mailbox it holds `send` on, with
  `confirm: true`, at most 100 a day; the message goes out under the address alone; its record is
  the key's own, and its owners and admins list its sends.

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
- Self-service sign-up, email verification, password recovery by email (the recovery code
  recovers a forgotten password, and the operator's reset invitation a lost recovery code too:
  [Passwords and sign-in](#passwords-and-sign-in)), passkeys.
- A screen for the server's own people: inviting someone to the server, disabling or deleting a
  person (`POST /v1/users/*`); the routes and the command line exist. A team's people have theirs
  (Members).
- OAuth for MCP clients: `/mcp` takes a key as a bearer.
- Renaming or pausing a mailbox, and replacing the stored password of a password mailbox.
- Periodic cleanup of expired sessions (they stay with the account; unused invitations are swept).
- The device-code flow in the console.
