# The server console and its API

The console is where a person signs in to **their** account on a Mailie server and connects their
own mailboxes: Gmail, Microsoft 365/Outlook, iCloud Mail and generic IMAP. The open console
(`web/`) has four sections: **Mailboxes**, **API keys & MCP**, **Storage** and **Account** (name,
password, sessions, and what they allow the server to do: sync and actions). No mail is read or
written there; a tool does that, with a key.

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
- **The console never shows the server's `message`.** It translates the `code` (and the operation
  it was doing) into its own words: English, and the Portuguese, Spanish, French and German
  catalogs in `web/src/ui/locales`.

### Editions

`web/src` is a **core** that every edition shares, plus the open edition in `web/src/open`. An
edition calls `configureEdition()` (`web/src/edition.ts`) before mounting: the console component
built on `components/ConsoleShell.vue`, the texts a person agrees to (sync, actions, a new key's
terms) with their revisions, a few sentences it words its own way, whether to show how to connect
over MCP, and optional extras (a byline, legal components, more translations). Other extension
points are `onLiveEvent()`/`onLiveLagged()` (`state/live.ts`), `addDescriber()` for an edition's own
operations (`ui/errors.ts`, `state/failure.ts`), `onActionsConsent()` and the account section's
pieces (`AccountPanel`, `SyncPermission`, `ActionsPermission`, `PermissionRow`).

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
stored as SHA-256, with an absolute lifetime of 14 days and no sliding renewal. The browser keeps
it in memory and an encrypted copy in IndexedDB under a non-extractable AES-GCM key, tells other
tabs over a `BroadcastChannel` when it signs out, and drops it on any `401`. Routes for a person
(`/v1/auth/*`) refuse API keys.

### Passwords and sign-in

Passwords are hashed with Argon2id on the server (64 MiB, t=3, p=1), at least 10 characters, with
at most two hashes running at once. An unknown address and a disabled person cost the same work as
a wrong password and get the same answer. Changing the password ends every other session.

### Invitations

Nobody signs up uninvited. An invitation is single-use, valid only for its address, for 7 days, and
records the role asked for.

- `mailserver user invite --email X [--role owner|member]` calls `POST /v1/users/invites` with
  `MAIL_ADMIN_KEY` (an unrestricted instance admin key; a signed-in owner may call the route too)
  and prints the link, `<MAIL_PUBLIC_URL>/#invite=…&email=…`. The code travels in the fragment,
  which the browser never sends to a server, and the page removes it as soon as it reads it.
- `--bootstrap` writes to the database directly, for the first person, while no daemon runs.
  Without `MAIL_PUBLIC_URL` its link points at `http://localhost:5174` and the command says so.
  Without `MAIL_PUBLIC_URL` the route answers `409`: a link is never built from a request's `Host`.
- The first person to sign up on a daemon becomes an `owner`, whatever the invitation said.
- Unused invitations are deleted within 30 days of expiring; a used one stays with the person and
  goes when they are deleted.

Closing someone's account is the operator's: `mailserver user disable --email X` ends their
sessions and revokes their keys; `mailserver user delete --email X` deletes the person, their
mailboxes with their credentials and index, their sessions, keys and invitation. Both take
`--email -` to read the address from standard input, which keeps it out of `sudo` logs and shell
history, and refuse to remove the last active owner without `--force`.

### Keys and scopes

A key is `<prefix>.<secret>`: eight hex characters, then 32 random bytes in base64url. Only its
Argon2id hash is stored; the prefix names it in lists and logs. Scopes are ordered,
`read < write < send < admin`. There are two kinds:

- **Instance keys**, the operator's: any scope, optionally restricted to some mailboxes
  (`--accounts`), at most 365 days. Over REST they see every mailbox; they never change a person's
  messages or send from a person's mailbox. They are managed through the `/v1/apikeys` routes,
  which the daemon mounts only with `MAIL_ADMIN_API=true` (off by default; otherwise they answer
  404). `mailserver apikey create --scope SCOPE --name NAME`, `apikey list` and
  `apikey revoke PREFIX` are clients of those routes, so they need it too. Without it the only way
  to issue one is `mailserver apikey create --bootstrap`, which writes to the database with the
  daemon stopped.
- **Personal keys**, which a person creates in the console for their own tools (see
  [API keys](#api-keys) below). They act as that person, and they are the only way a tool reaches
  a person's mailbox.

A session counts as a person with every scope; what it may do to a mailbox still depends on
ownership and consent.

### Rate limits

Sign-in and sign-up: 60 a minute per address (burst 20) and 5 a minute per email address, before
any hashing; a password change, 5 a minute per person. Authenticated requests spend from a bucket
per address (600 a minute, burst 60). A wrong key secret spends from a tight bucket for that key's
prefix (30 a minute, burst 10), and a key whose prefix does not exist from a tight bucket for the
address, consulted only once the prefix is not found. No failure is charged to the address as a
whole: behind a NAT, or a proxy missing from `MAIL_TRUSTED_PROXIES`, one address is a whole office.
IPv6 addresses count per /64. A throttled request gets `429` with `Retry-After`.

## Ownership

Every mailbox has at most one owner (`accounts.owner_user_id`). Whoever connects it in the console
owns it; one created with an instance key (the command line) has no owner. Roles:

- **`owner`** runs the instance: sees, besides their own, the mailboxes nobody owns, and invites
  people.
- **`member`** sees only their own mailboxes.

The rule lives in `internal/service` and is applied in SQL, so listing filters with the same rule
as fetching one. Another person's mailbox answers `not_found`, never `forbidden`. A mailbox address
is unique across the daemon: a mailbox is connected once.

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
| `POST /v1/auth/signup` | anyone | `{invite, email, name, password}` → `Session` (201) |
| `GET /v1/auth/me` | session | `{user, session}` |
| `POST /v1/auth/logout` | session | `{everywhere?}` → 204 |
| `POST /v1/auth/password` | session | `{current, next}` → a new `Session`; every other session ends |
| `PUT /v1/auth/profile` | session | `{name}` → `User` |
| `POST /v1/users/invites` | owner, or unrestricted instance admin key | `{email, role?}` → `Invite` |
| `POST /v1/users/disable` | unrestricted instance admin key | `{email, force?}`: sessions ended, keys revoked |
| `POST /v1/users/delete` | unrestricted instance admin key | `{email, force?}` → what was deleted |
| `GET /v1/providers` | read | which providers and flows **this** caller can use, the default first |
| `GET /v1/accounts`, `GET /v1/accounts/{id}` | read | the mailboxes the caller sees |
| `POST /v1/accounts` | admin (a session counts) | add a mailbox (45 s) |
| `DELETE /v1/accounts/{id}` | admin | remove it, with its index |
| `POST /v1/accounts/{id}/oauth/start` | admin | start (or restart) its authorization |
| `POST /v1/accounts/oauth/callback` | admin | `{redirect_url}` → `Account` (45 s) |
| `GET /v1/accounts/{id}/folders` | read | folders, from the index or from the server |
| `GET /v1/accounts/{id}/sync` | read | `AccountSync` |
| `POST /v1/accounts/{id}/sync` | write | ask for a pass now → `AccountSync` (202) |
| `PUT /v1/accounts/{id}/sync` | unrestricted instance admin key | `{enabled}`, only for a mailbox nobody owns |
| `GET/POST/DELETE /v1/me/sync-consent` | see [Sync](#sync-and-consent) | the person's consent to sync |
| `GET/POST/DELETE /v1/me/actions-consent` | see [Actions](#actions) | the person's consent to actions |
| `GET/POST/DELETE /v1/me/send-consent` | see [Sending](#sending) | the person's consent to sending |
| `GET /v1/me/storage` | read | what the caller's mailboxes take up in the index |
| `GET /v1/me/mcp` | read | `{http}`: whether this server answers MCP over HTTP at `/mcp` |
| `GET/POST /v1/me/apikeys`, `DELETE /v1/me/apikeys/{prefix}` | session | the person's own keys |
| `GET /v1/events` | read | Server-Sent Events of the mailboxes the caller sees |
| `GET /v1/events/wait` | read | long poll for new mail |
| `GET /v1/messages` | read | search the index |
| `GET /v1/messages/{id}` | read | a message, its body fetched from the mail server now |
| `GET /v1/messages/{id}/raw` | read | the original (`message/rfc822`), always as an attachment |
| `GET /v1/messages/{id}/attachments/{path}` | read | one part, by its IMAP section, always as an attachment |
| `PATCH /v1/messages/{id}`, `POST /v1/messages/{flags,move,trash}` | write | actions |
| `POST /v1/messages/send` | send | send a message |
| `GET /v1/sends/{key}?account=` | send | a send's record |
| `GET/POST /v1/apikeys`, `DELETE /v1/apikeys/{prefix}` | admin, only with `MAIL_ADMIN_API=true` | instance keys; `mailserver apikey create\|list\|revoke` without `--bootstrap` call these |

`/v1/users/disable` and `/v1/users/delete` carry the address in the body, never in the URL.

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

Nothing from a person's messages is stored until they agree in the console, and turning sync off
deletes what was stored:

- **The consent is the person's**, given once for every mailbox they own
  (`users.sync_consent_at` and `users.sync_consent_version`). Connecting and authorizing a mailbox
  is **not** consenting. Only a session gives or withdraws it (`POST`/`DELETE` with any key is
  `403`); a person's key may read it (`GET`); an instance key may not (`403`: there is no person to
  answer for).
- **The revision is checked.** `POST /v1/me/sync-consent` requires `{"version": "<revision>"}`, the
  revision of the text the console showed. Another revision, none, or another field name is `400`.
  Agreeing again to the same revision keeps the first date. The answer has `current_version`: the
  console compares it with `version` to ask again when the text changes, and with its own text's
  revision so it never agrees to a text it did not show.
- **Having agreed to an earlier revision does not stop sync**: eligibility looks only at
  `sync_consent_at`. A mailbox whose owner agreed to an earlier revision keeps syncing, and its
  index keeps being served, until the owner turns sync off.
- **Withdrawing deletes.** `DELETE /v1/me/sync-consent` clears the consent and, **in the same
  transaction**, deletes everything sync stored for the person's mailboxes: messages (with their
  parts and full-text rows), folders and those mailboxes' events. Mailboxes, credentials and
  settings stay. After the commit the daemon compacts the full-text index and checkpoints the WAL,
  so the deleted words are gone from the files too. The engine checks eligibility inside every
  transaction that writes to the index, so a batch already on its way writes nothing.
- **A mailbox nobody owns** has no person to consent: it syncs only when the operator switches it
  on, `mailserver account sync ID on` (`PUT /v1/accounts/{id}/sync {"enabled": true}`, unrestricted
  instance admin key). `off` asks for confirmation and deletes its index. For a person's mailbox the
  route answers `409`: nobody decides for them, not the operator either.
- **The rule the engine reads** (`internal/store/eligibility.go`): the account is `active` and,
  with an owner, the owner is active and has consented; without one, the operator switched it on.

The first sync of a person's mailbox reaches back 90 days and then follows new mail. Gmail's All
Mail, Starred and Important are never synced: they are views of other folders.

`SyncConsent` is `{consented, version?, consented_at?, current_version}`.

### An account's sync

Every account in the JSON has `sync`, the `AccountSync` that `GET /v1/accounts/{id}/sync` answers:

| Field | |
|---|---|
| `enabled` | sync is allowed: the owner consented or, without one, the operator switched it on |
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
| `MAIL_CONSENT_VERSION_SYNC` | `2026-10-open-sync` | what sync stores (`web/src/open/SyncText.vue`) |
| `MAIL_CONSENT_VERSION_ACTIONS` | `2026-10-open-actions` | the server changing their mailboxes (`web/src/open/ActionsText.vue`) |
| `MAIL_CONSENT_VERSION_SEND` | `2026-10-open-sending` | sending from their mailboxes (the open console has no such text) |
| `MAIL_CONSENT_VERSION_KEYS` | `2026-10-open-api-keys` | what a tool holding a new key can do (`web/src/open/KeyTermsText.vue`) |

The defaults are the open console's texts, held to `web/src/open/versions.ts` by
`web/test/contract.spec.ts`: moving a default without a new text fails the build. A server that
serves another console sets the revisions its texts carry. The console sends the revision of the
text it showed, never `current_version`, and the daemon accepts only the configured one. Changing a
revision asks everybody again: a consent to another revision of actions or sending stops counting
(they are refused) until the person agrees to the new text; a consent to another revision of sync
keeps their mailboxes syncing unless they turn it off; a key keeps the terms it was created under.
Values are printable ASCII without spaces, at most 64 bytes. A console whose texts carry other
revisions than the daemon's sees every agreement refused, and offers only a reload.

## Storage

`GET /v1/me/storage` (read scope, any credential) is what the caller's own mailboxes take up in the
index: `{mailboxes: [{account_id, email, messages, bytes}], total: {messages, bytes},
database_bytes?}`, sorted by address.

- **Whose mailboxes**: the rule of `GET /v1/accounts`, except that an instance key answers only for
  the mailboxes nobody owns, never a person's. A mailbox never synced, or whose index was deleted,
  is listed with zeros; one whose account needs signing in again keeps, and reports, what was
  already indexed.
- **Per folder copy**: `messages` counts the index rows still in their folder and `bytes` sums their
  `RFC822.SIZE`, so a Gmail message under three synced labels counts three times. `bytes` is the
  mail's size on its server, not what the index keeps of it.
- **`database_bytes`**, only for an `owner` signed in: `mail.db` plus `mail.db-wal` on disk,
  everybody's data included.

## Events: SSE and long poll

Both read the event journal (`events`) from a cursor and deliver only events of mailboxes the caller
sees, decided in `internal/service` **per event**: a mailbox connected while the stream is open
appears from its first event, without reconnecting. No payload carries a token or a credential;
they may carry subjects and senders, which are the person's own.

**`GET /v1/events`** (SSE, no route timeout):

- Each event is `id: <seq>`, `event: <type>`, `data: <Event as JSON>`, with
  `Event = {seq, type, account_id, at, payload}`: the same shape as the long poll's items.
- It resumes after `Last-Event-ID` or `?since=`; the header wins. With neither it starts **from
  now**: the journal is for resuming a stream, not for reading the mailbox.
- Filters: `?account=ID[,ID]` (someone else's is `404`) and `?types=message.new,…` (an unknown type
  is `400`).
- `: ping` every 15 s. The credential is **checked again before each batch** and at each ping: an
  ended session or a revoked key receives `event: error` with `{code, message}` instead of the next
  event, and the stream ends. Do not reconnect after that.
- `event: lagged` (no `id`) comes first when the cursor is older than the journal keeps (7 days,
  and always the latest 10,000 events): what survives follows, but the client must read again the
  state it keeps.
- A slow client loses nothing: the stream waits for it, and what it has not read stays in the
  journal. When a stream ends on its own (the daemon restarted), reconnecting with the last `id`
  as `Last-Event-ID` loses nothing.
- A browser should use `fetch` with the bearer in the header, never `EventSource` (which sends no
  header) and never a token in the URL. The daemon sends `X-Accel-Buffering: no`; a proxy in front
  must not buffer.

**`GET /v1/events/wait?since&timeout&account`** is a long poll for **new mail**: `message.new` in an
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
| `send.finished` | `{account_id, key, state}` |

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
2. Every message must be readable by the caller; one of someone else's, or one that does not exist,
   makes the whole request `404`. All in one account, 1 to 100 ids.
3. **A person's mailbox**: the caller must **be** that person (their session or a key they created)
   and their consent to actions must name the current revision. An instance key gets `403`; without
   consent, or with an old one, `409`.
4. **A mailbox nobody owns**: an instance key with `write`, or an owner signed in.
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
4. **A person's mailbox**: the caller must be that person, with their consent to sending at the
   current revision. An instance key gets `403`; without consent, `409`.
5. **A mailbox nobody owns**: an instance key with `send`, or an owner signed in.
6. The account must be usable (`409` otherwise).

Each account's JSON has `send: {available, reason?, from_name?}`: whether it can send **for this
caller**, consent aside. `reason` is `needs_reauth`, `pending_auth`, `disabled`, `no_smtp` or
`not_owner`.

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

- The sender is the account's address, under its owner's name (the address alone for a mailbox
  nobody owns).
- A reply sets `In-Reply-To` and `References` from the original, and `Re: ` once. After it is sent,
  the original gets `\Answered` only if its owner allowed actions.
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
(check Sent before sending again). The same key with another message is `409`. `failed` frees the
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

**`GET /v1/sends/{key}?account=`** (send scope; still readable after withdrawing consent) answers
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
- `account_ids`: mailboxes the person sees; someone else's is `404` and nothing is created.
- `terms_version` must be `MAIL_CONSENT_VERSION_KEYS`; another is `409`. A key keeps working under
  the terms it was created with.
- At most **20 live keys** per person; the 21st is `409`.
- The secret appears **once**, in the `POST` answer (`Cache-Control: no-store`).
- The list shows every key that acts as the person, so they can revoke it, including one an
  administrator issued for them before personal keys existed (`terms_version` empty). Such a key
  works on no route, REST or `/mcp`.
- A key restricted to mailboxes is **revoked** when its last mailbox is removed, rather than becoming
  a key for every mailbox.

The console shows the MCP address (`<origin>/mcp`) and the Claude Code command only when
`GET /v1/me/mcp` says the server answers there.

## Running the console locally

Prerequisites: Go 1.27, Node 24, and a `.env` with `MAIL_CREDENTIAL_KEY_HEX` (see the README).

```sh
echo 'MAIL_PUBLIC_URL=http://localhost:5174' >> .env
make build
./bin/mailserver user invite --bootstrap --email you@example.com   # daemon stopped
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
- Self-service sign-up, email verification, password recovery by email, passkeys.
- A screen to invite and manage people; the routes and the command line exist.
- OAuth for MCP clients: `/mcp` takes a key as a bearer.
- Renaming or pausing a mailbox, and replacing the stored password of a password mailbox.
- Periodic cleanup of expired sessions (they stay with the account; unused invitations are swept).
- The device-code flow in the console.
