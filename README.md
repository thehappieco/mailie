# Mailie

Mailie is a mail server for your own tools: one Go binary that connects several mailboxes at once
(Gmail and Microsoft 365 over OAuth2, iCloud Mail and any IMAP/SMTP server with a password) and
gives them a single **REST API** and an **MCP server**, so a script or an AI assistant can search,
read, act on and send mail with a key you control.

- **A local index, not a copy of your mail.** Once you turn a mailbox's sync on, the server indexes
  its metadata (senders, recipients, subjects, dates, sizes, folders, flags) into SQLite, with
  full-text search over subject, sender and recipients. Message bodies, attachments and originals
  are fetched from the mail server when someone asks for them and are never stored.
- **New mail in real time.** IMAP IDLE feeds an event journal, served as Server-Sent Events, a
  long poll and the MCP tool `wait_for_new_mail`.
- **Actions on the real mailbox.** Mark read or unread, star, archive, move and trash, on the mail
  server itself, with a key allowed to act. Nothing is ever deleted for good, and reading a message
  never marks it as read.
- **Sending.** Plain-text messages, replies and forwards with attachments, through each mailbox's
  own SMTP server. Every send needs an explicit `confirm: true`, an idempotency key is reserved
  before the server is dialed, and a send whose outcome is unknown is never retried.
- **One service, two transports.** REST and MCP are thin adapters over the same service layer, so
  the same rules decide who sees what, whichever way a tool comes in.
- **Run from its command line.** No web screen: you add mailboxes, switch their sync and make keys
  with `mailserver`, and your tools use REST and MCP with those keys.
- **Small to run.** No cgo: one static binary, one SQLite file, one data directory.

## Status

Mailie is under active development and has no stable release yet; the REST API and the MCP tools
may still change.

| Works today | Not yet |
|---|---|
| Gmail, Microsoft 365 and Outlook.com (OAuth2), iCloud Mail, generic IMAP | Drafts kept on the server |
| Sync switched on per mailbox, search, reading on demand | Search in message bodies |
| Read/unread, star, archive, move, trash, undo | Composing HTML |
| Sending over REST, with attachments, replies and forwards | MCP tools that send |
| MCP over Streamable HTTP and stdio, with read and action tools | OAuth for MCP clients (claude.ai web connectors) |
| The command line: mailboxes, sync, keys, backups | Webhooks |
| Encrypted database backups to S3 | Replacing a mailbox's stored password |
| A Docker image, Compose and a hardened systemd unit for self-hosting | |

## Quick start

You need Go 1.27, or only Docker for the image ([`docs/self-hosting.md`](docs/self-hosting.md)).
`make help` lists every target.

```sh
git clone https://github.com/thehappieco/mailie.git && cd mailie
cp .env.example .env
openssl rand -hex 32            # paste it into MAIL_CREDENTIAL_KEY_HEX in .env
make build                      # bin/mailserver
```

`MAIL_CREDENTIAL_KEY_HEX` encrypts every stored refresh token and password. The server refuses to
start without it. **Keep a copy with your other secrets: losing it means authorizing every mailbox
again.**

Then an admin key for the command line, the daemon, and a mailbox:

```sh
# Like every --bootstrap command, this opens the database directly and refuses to run while the
# daemon is up.
./bin/mailserver apikey create --bootstrap --scope admin --name cli
export MAIL_ADMIN_KEY=<the key it printed>
./bin/mailserver serve &        # 127.0.0.1:8080

# A password mailbox: the password is typed at a prompt that does not show it, or piped in.
./bin/mailserver account add --email you@icloud.com --password -
# A Gmail or Microsoft mailbox, through the installed OAuth client (below): opens the provider's page.
./bin/mailserver account add --email you@gmail.com

./bin/mailserver account list
./bin/mailserver account sync <account id> on       # nothing is indexed until you turn it on
./bin/mailserver account sync <account id> status
curl -H "Authorization: Bearer $MAIL_ADMIN_KEY" 'http://127.0.0.1:8080/v1/messages?q=invoice'
```

The command-line tools are REST clients of the running daemon and authenticate with
`MAIL_ADMIN_KEY`. `account add` says where the new mailbox's sync stands, for instance
``added you@icloud.com as acc_…; its sync is off until `mailserver account sync acc_… on` ``.

`--password` takes only `-`: never the password itself, which `sudo`, `systemd-run` and shell
history record. At a terminal the password is typed at a prompt that does not show it; otherwise
it is the first line of standard input (`printf '%s\n' "$APP_PASSWORD" | mailserver account add
--email you@icloud.com --password -`), with only its line ending taken off and nothing after it
read, at most 4096 bytes. Any other value is refused before anything is sent. The daemon logs in
once with the password before it stores it, so a wrong one is refused and stored nowhere.

Give each tool a key of its own, with the scope it needs, rather than the admin key:
`mailserver apikey create --scope read --name assistant [--accounts IDS]`. It calls the daemon's
`/v1/apikeys` routes, which exist only with `MAIL_ADMIN_API=true` in the daemon's environment;
without them, `apikey create --bootstrap` issues a key with the daemon stopped. A tool then speaks
REST ([The API](#the-api)) or MCP ([MCP](#mcp)) with it.

## Mailboxes and providers

The command line links Gmail and Microsoft mailboxes through **installed OAuth clients** you
register yourself (`MAIL_GOOGLE_CLIENT_ID`, `MAIL_MICROSOFT_CLIENT_ID`); they are the only way to
link them on a self-hosted server. A registration identifies **your server**, not a mailbox: one
serves every account of its provider, and whoever owns a mailbox consents for it in a browser.

- For each authorization the daemon opens a listener on its own loopback, and the provider sends
  the browser back there (`http://127.0.0.1:<port>` for Google, `http://localhost:<port>` for
  Microsoft). `account add` opens that browser: it must run on the daemon's machine.
- On a server, `account add --paste` prints the provider's address instead: open it in a browser
  anywhere, consent, and paste back the address the browser ends on, which looks like a connection
  error. `account authorize ID [--paste]` does the same when a grant has to be renewed.
- Each account remembers the client that issued its grant and always refreshes with it.

The daemon also takes **web clients** (`MAIL_GOOGLE_WEB_CLIENT_ID`/`_SECRET`,
`MAIL_MICROSOFT_WEB_CLIENT_ID`/`_SECRET`), which redirect to `<MAIL_PUBLIC_URL>/oauth/return`, a
page of a web console. A self-hosted server needs none: they serve an edition that has a web page,
and the mailboxes an older release's console linked through them, which refresh only with them
until they are linked again ([Moving from the open console](#moving-from-the-open-console)).

### Google

1. In the [Google Cloud Console](https://console.cloud.google.com/), create a project and
   configure the **OAuth consent screen** (type *External*).
2. Under **Data access**, add the scope `https://mail.google.com/`, the one IMAP and SMTP accept,
   which Google lists as *restricted*. The command line asks for it alone.
3. Set the publishing status to **In production**, even unverified. In *Testing*, every refresh
   token expires after 7 days, so every Gmail mailbox would need authorizing again each week. An
   unverified app shows Google's "unverified app" screen and is limited to 100 users over the
   project's life, which is plenty for yourself and your team; going past that takes Google's
   verification of the restricted scope.
4. **Credentials → Create credentials → OAuth client ID → Desktop app.** There is no redirect to
   register: Google accepts the loopback address. Put the id (and the secret Google shows, which
   protects nothing for a desktop client) in `MAIL_GOOGLE_CLIENT_ID` and
   `MAIL_GOOGLE_CLIENT_SECRET`.

Google allows 15 simultaneous IMAP connections per account across all of its clients (Mailie uses
at most three per mailbox), and changing the account's password revokes the refresh token.

### Microsoft 365 and Outlook.com

1. In the [Entra admin center](https://entra.microsoft.com/), **App registrations → New
   registration**, for "Accounts in any organizational directory and personal Microsoft
   accounts". `MAIL_MICROSOFT_TENANT` stays `common`.
2. **API permissions → Add → APIs my organization uses → Office 365 Exchange Online → Delegated**:
   `IMAP.AccessAsUser.All` and `SMTP.Send`. If the tenant restricts user consent, an administrator
   grants them. The daemon also asks for `offline_access`.
3. **Authentication → Add a platform → Mobile and desktop applications**, redirect
   `http://localhost` (Entra ignores the port; `127.0.0.1` and `[::1]` are not accepted), and
   **Allow public client flows: Yes**. A public client has no secret. Its id goes in
   `MAIL_MICROSOFT_CLIENT_ID`.

Microsoft refuses passwords for IMAP, so Microsoft mailboxes connect with OAuth only. In a work
tenant, each mailbox needs IMAP and, for sending, SMTP AUTH enabled:

```powershell
Set-CASMailbox -Identity user@contoso.com -ImapEnabled $true -SmtpClientAuthenticationDisabled $false
```

Security defaults and Conditional Access must allow IMAP and SMTP with modern authentication. A
personal Outlook.com account that answers `BAD User is authenticated but not connected` needs
"Let devices and apps use IMAP" turned on in its settings. The device-code flow
(`account add --device`, `MAIL_MICROSOFT_DEVICE_CODE=true`) exists for machines without a browser,
but Entra security defaults block it: `--paste` is the way on a server.

### iCloud Mail and other IMAP servers

Apple offers no OAuth to third-party apps. An iCloud mailbox connects with an **app-specific
password** (it needs two-factor authentication: account.apple.com → Sign-In and Security →
App-Specific Passwords); the daemon knows Apple's servers. A custom iCloud+ domain signs in with the
account's `@icloud.com` address as the user name:

```sh
mailserver account add --email you@icloud.com --password -
mailserver account add --email you@yourdomain.com --provider icloud \
  --login-user you@icloud.com --password -
```

Any other IMAP server takes its own host, port and password (`--imap-host`, `--smtp-host`,
`--smtp-tls`, …). Every password account logs in once before it is saved. IMAP and SMTP always
use TLS. A host that resolves to a loopback, private, link-local or CGNAT address is refused when
the daemon dials it unless `MAIL_ACCOUNT_ALLOW_PRIVATE=true`, so a host someone names cannot reach
your internal network. No route replaces a stored password yet: when an app-specific password
changes, remove the mailbox, delete it for good and add it again.

## The operator's mailboxes

The mailboxes the command line adds belong to the **operator workspace**, which only instance keys
reach. As the operator, with `MAIL_ADMIN_KEY`:

- `mailserver account list` lists them with their state; `account rename ID NAME` names one,
  `account folders ID` lists its folders, and `account authorize ID` renews a grant that ended.
- **Sync.** Nothing of a mailbox is stored until `mailserver account sync ID on`. Its first pass
  reaches 90 days back (`account add --initial-days N` reaches further, a negative number
  everything), and new mail follows. `account sync ID status` says how far it got and `now` asks
  for a pass; `pause` keeps the index, read and searched, while nothing new is indexed, until
  `resume`; `off` deletes the index.
- **Removing.** `mailserver account remove ID` keeps a mailbox 30 days, used by nothing, and then
  deletes it for good with its index; within them `account recover ID` brings it back, its sync
  paused until `account sync ID resume`, and `account purge ID` deletes it for good sooner. A
  link that never connected is deleted at once. `account removed` lists the removed ones
  ([`docs/workspaces.md`](docs/workspaces.md#removed-mailboxes)).
- **What they hold.** `GET /v1/me/storage` says what each mailbox holds in the database, by a
  versioned rule (`rule_version`), its removed ones included until they are deleted for good, and
  to the operator the database's own size ([`docs/console.md`](docs/console.md#storage)).

The daemon also keeps people, personal workspaces and teams, which the people commands (`user`,
`workspace`, `member`, `access`) administer: a database made through the open console of earlier
releases holds them ([Moving from the open console](#moving-from-the-open-console)). Nobody signs
in to a self-hosted server: it has no web screen.

## Moving from the open console

Earlier releases served a web console where the people you invited signed in and linked their own
mailboxes, into their personal workspace or a team. A mailbox belongs to the workspace it was
linked into and never changes workspace, so an instance key does not reach those mailboxes. At
every start, while the database holds any, the daemon logs one warning with their counts, never an
address or an id:

```
time=… level=WARN msg="mailboxes outside the operator workspace, in personal or team workspaces: no instance key reaches them" personal_live=2 personal_removed=0 team_live=1 team_removed=1 hint="to reach them with an instance key, link each address again into the operator workspace: see \"Moving from the open console\" in the README"
```

`personal_removed` and `team_removed` count the removed mailboxes, kept until they are deleted for
good. A daemon that cannot count them says so at warn and starts anyway.

To move them, link each address again into the operator workspace, and only then delete the
people:

1. **Make an instance key**, if you have none: `mailserver apikey create --bootstrap --scope admin
   --name cli` with the daemon stopped, then `export MAIL_ADMIN_KEY=<the key>`.
2. **Link each address again** into the operator workspace with `mailserver account add --email
   ADDRESS` (Gmail and Microsoft through the installed clients [above](#mailboxes-and-providers),
   iCloud and IMAP with `--password -`), turn its sync on with `mailserver account sync ID on`, and
   check that it syncs (`account sync ID status`, `account list`). An address is linked at most
   once per workspace, so the new mailbox lives beside the old one, in another workspace.
   `mailserver workspace list`, `member list --workspace ID` and `access list --workspace ID` name
   the workspaces, their people and their mailboxes. While both exist, a Gmail account holds up to
   six of its 15 connections.
3. **Only then delete the people**, members before owners, each with
   `mailserver user delete --email - --force` and the address on standard input
   (`printf '%s\n' ADDRESS | mailserver user delete --email - --force`). It runs through the daemon
   with `MAIL_ADMIN_KEY` (an instance key with the admin scope, restricted to no mailbox), or with
   `--bootstrap` while the daemon is stopped. Each deletion removes, for good and at once, the
   person, the mailboxes of their personal workspace (live and removed) with their credentials and
   index, their sessions, keys and invitations, and every team they were the only member of, with
   that team's mailboxes; a team others are still in keeps its mailboxes until its last member
   goes. `--force` overrides the refusals that protect whoever remains: the server's last active
   owner, a team's last active owner, the last person who can read a team mailbox (a removed one
   kept for recovery included), and the person whose own consent a team mailbox still syncs under
   since the upgrade that gave teams their mailboxes (that sync stops, and its index is deleted).
   Linking first matters for Google: deleting a Gmail mailbox for good revokes its grant at
   Google, which Google keeps one of per account and Cloud project, and the daemon skips that while
   another mailbox here holds the same account.
4. **Check that the start-up warning is gone**: restart the daemon and look for the line. It goes
   once no personal or team mailbox is left, live or removed: deleting their people deletes theirs,
   and `account purge ID` deletes any removed mailbox for good (`account removed --workspace ID`
   lists a workspace's). Then the web client's variables, `MAIL_PUBLIC_URL` and `MAIL_WEB_DIR` can
   leave the environment, with the old console's files.

A re-link keeps the mail, which the provider holds, and loses everything Mailie held about the
mailbox:

- the index older than the new first sync, 90 days back unless `--initial-days` says otherwise;
- every message, folder and event id, and every cursor a client holds (an event stream's, a
  search's next page);
- who held what on it: grants and flags, its mailbox key and the grants sealed of it, and what
  every API key held on it, keys an issuer made included;
- the notices about it and its entries in the mailbox journal;
- its send records, with their idempotency keys;
- its name, and a team's agreement to sync it and its pause.

The same way moves a mailbox between a self-hosted server and the hosted service: link it on the
other side, check that it syncs, then remove it where it was. Nothing of the index moves, only the
mail the provider keeps. A mailbox removed on one side keeps its address taken in its workspace
there for 30 days, so linking it back within them needs a recovery or deleting it for good first,
and each server that links a Gmail account holds up to three of its 15 connections.

## The API

Every route is under `/v1` and takes `Authorization: Bearer <key>`. Errors are `{"code",
"message"}` with one of seven codes. A few examples:

```sh
H="Authorization: Bearer $KEY"
curl -H "$H" localhost:8080/v1/accounts
curl -H "$H" 'localhost:8080/v1/messages?q=invoice&unseen=true&limit=20'
curl -H "$H" localhost:8080/v1/messages/1284                        # fetched from the mail server now
curl -H "$H" 'localhost:8080/v1/events/wait?timeout=50'             # long poll for new mail
curl -H "$H" -N localhost:8080/v1/events                            # Server-Sent Events
curl -H "$H" -H 'Idempotency-Key: 7d1c0e1a' localhost:8080/v1/messages/send \
  --form-string 'compose={"account_id":"acc_…","to":[{"email":"bea@example.org"}],"subject":"Hello","text":"Hi Bea","confirm":true}'
```

[`docs/console.md`](docs/console.md) is the REST reference: every route, its JSON, consent,
events, storage and the OAuth flows.

### Keys and scopes

| Scope | Allows |
|---|---|
| `read` | listing, searching, reading bodies, downloading attachments and originals, waiting for new mail. It never marks anything as read. |
| `write` | the above, plus flags, moves, archive and trash. A workspace key acts only with `act` on the mailbox, under the key terms its creator agreed to. |
| `send` | the above, plus sending. A workspace key sends only with `send` on the mailbox, under the address alone, unless the server says keys may not send (`MAIL_KEYS_MAY_SEND=false`). Every send needs `confirm: true`. |
| `admin` | the above, plus mailboxes, keys and people. |

There are two kinds of key:

- **Instance keys**, for the operator: `mailserver apikey create --scope SCOPE --name NAME
  [--accounts IDS] [--days N]`, at most a year, optionally restricted to some of the operator
  workspace's mailboxes. Over REST and MCP alike they reach only those, the operator workspace's,
  never a person's or a team's.

  `apikey create`, `apikey list` and `apikey revoke PREFIX` call the daemon's `/v1/apikeys` routes,
  which exist only while it runs with `MAIL_ADMIN_API=true` (off by default; otherwise the daemon
  answers 404). Without them, `apikey create --bootstrap` issues a key straight into the database,
  with the daemon stopped, as for the first key above; listing and revoking need the routes.
- **Workspace keys** belong to a person's or a team's workspace, and reach exactly the mailboxes of
  it they were given, each with `read`, `act` or `send`: `read`, `write` or `send`, for 30, 90 or
  365 days, at most 20 alive per workspace. Its owners and admins create them signed in, which a
  self-hosted server, with no web screen, no longer offers; a database from the open console keeps
  the ones it holds until they expire or are revoked (`mailserver apikey revoke PREFIX` revokes any
  key). See [`docs/workspaces.md`](docs/workspaces.md#api-keys).

Keys are stored only as Argon2id hashes and shown once. Revoked keys stay listed, because they
answer "what could have read this mailbox". An instance key restricted to mailboxes that have all
been removed is revoked rather than widened to every operator mailbox; a workspace key left with no
mailbox reaches nothing.

## MCP

The daemon is an MCP server: Streamable HTTP at `/mcp` and stdio. Tools to read (`list_accounts`,
`list_folders`, `search_messages`, `get_message`, `get_attachment`, `wait_for_new_mail`) and to act
(`mark_read`, `flag_message`, `move_message`, `trash_message`), and `mail://` resources. MCP takes
API keys only, never a sign-in session's token: on a self-hosted server, an instance key from
`mailserver apikey create`, which `--accounts` restricts to the mailboxes the tool may use.

Connecting a client, from the machine it runs on (the same binary, which needs nothing of the
daemon's there):

```sh
# Claude Code speaks HTTP with a header itself:
claude mcp add --transport http --scope user mailie https://mail.example.com/mcp \
  --header "Authorization: Bearer <your key>"

# Claude Desktop, through a local bridge; Cursor, over HTTP. The key is typed (not shown) or piped,
# checked against the server, and written only into the client's file, mode 0600:
mailserver mcp install --client claude-desktop --url https://mail.example.com
mailserver mcp install --client cursor --url https://mail.example.com

# Any client that launches a local server: a stdio bridge to the server's /mcp.
MAILIE_API_KEY=<your key> mailserver mcp connect --url https://mail.example.com
```

Addresses are `https://`, or `http://` only to the same machine (`http://localhost:8080`).
`MAIL_MCP_HTTP=false` switches the HTTP endpoint off (`/mcp` then answers 404, and the bridge says
so), and `mailserver serve --mcp-stdio` with `MAIL_MCP_KEY` speaks MCP on standard input and output
for a client that launches the whole daemon itself. Everything else — what `mcp install` writes where,
taking a key back, limits and what is logged — is in [`docs/mcp.md`](docs/mcp.md).

## Running it for real

[`docs/self-hosting.md`](docs/self-hosting.md) takes a server from a checkout to running: with
Docker Compose, Docker or a hardened systemd unit ([`deploy/`](deploy)), behind Caddy or nginx,
with the first key, OAuth clients, backups and upgrades. In short:

- The daemon listens on `127.0.0.1:8080` and speaks plain HTTP. To reach it from elsewhere, put a
  reverse proxy that terminates TLS in front, set `MAIL_TRUSTED_PROXIES` to the proxy's address
  (otherwise every client shares one rate limit) and `MAIL_ENV=prod`. Turn response buffering off
  for `/v1/events` and `/mcp`, which stream.
- With `MAIL_WEB_DIR` empty, as the image and the systemd unit leave it, it serves the API only:
  REST, MCP and `/metrics`, and `GET /` answers 404.
- `/metrics` (Prometheus) is served on the main listener unless `MAIL_METRICS_ADDR` gives it one
  of its own; keep it off the public proxy.
- One daemon per data directory: a lock refuses a second one. `mailserver migrate` applies schema
  migrations; the daemon also applies them when it starts.
- To rotate `MAIL_CREDENTIAL_KEY_HEX`, set the new key with a new `MAIL_CREDENTIAL_KEY_ID`, keep the
  old one in `MAIL_CREDENTIAL_PREVIOUS_KEYS` (`<id>:<hex>`), and run
  `mailserver rewrap-credentials` with the daemon stopped: it re-seals the credentials, the
  send-hash root (the key of a send record's hashes) and the salt key (the key of the salts
  passwords are derived under) under the new key. The daemon then no longer
  needs the old key (it refuses to start while the root needs one it is not given), but do not
  destroy it: `rewrap-credentials` re-seals the live database only, so every backup
  taken before the rotation still holds credentials sealed under the old key. Keep each retired
  key, with its id, beside your other secrets for as long as any backup sealed under it is kept
  ([`docs/backup.md`](docs/backup.md#putting-a-restored-database-into-service)).
- Configuration is environment variables only (`MAIL_*`, optionally from a `.env` in the working
  directory); [`.env.example`](.env.example) documents each one.

### Backups

`mailserver backup` takes a consistent snapshot beside the running daemon, encrypts it under an AWS
KMS data key and uploads it to S3; `mailserver backup restore` verifies and decrypts one. It is
optional, and designed so that the host can create backups but never read or delete them. See
[`docs/backup.md`](docs/backup.md).

## Security model

- **What the server holds.** One OAuth refresh token or password per mailbox, encrypted with
  AES-256-GCM under `MAIL_CREDENTIAL_KEY_HEX` and bound to its account and field, and the send-hash
  root, sealed the same way; the index of metadata of synced mailboxes; Argon2id hashes of keys;
  SHA-256 hashes of session tokens. Message bodies and attachments are never stored: while one is
  being served it sits in a `0600` temporary file that is removed when the response ends.
- **Who can read it.** Whoever has both the database and `MAIL_CREDENTIAL_KEY_HEX` can reach the
  mailboxes; whoever runs the server can read the index. The key lives in the environment, which
  protects copies and backups of the database, not against code running as the daemon's user. On
  EC2 an AWS KMS key can seal the credentials instead (`MAIL_CREDENTIAL_KMS_KEY_ARN`, a data key
  per credential): no key is then in the environment, and every unwrapping is a call KMS's policy
  allows and CloudTrail records ([`docs/self-hosting.md`](docs/self-hosting.md#credentials-under-aws-kms)).
- **Who sees what.** Instance keys belong to the operator and reach only the operator workspace's
  mailboxes (with `--accounts`, only some of them), over REST and MCP alike. A mailbox of a
  person's or a team's workspace is `not_found` to them, never `forbidden`, and so it is to
  everyone who holds nothing on it. Acting needs the `write` scope and sending the `send` scope,
  every send with `confirm: true`, and who may is asked again before every command that changes
  the mailbox and before every connection to the submission server.
- **No local exception.** A process on the same machine still needs a key. Rate limits apply per
  address and to failed attempts per key prefix; failures never lock out a whole address.
- **Logs** carry ids, never subjects, addresses, bodies, tokens or passwords.

Report vulnerabilities as described in [`SECURITY.md`](SECURITY.md).

## Editions

This repository is the **self-hosted edition**, under Apache-2.0: the server and its command line,
used through REST and MCP, with no web screen; and, in [`web/`](web), the core of a web console
that an edition with a web page compiles. Running it needs no account or subscription, and it
talks to nobody but your mail providers (and AWS, if you configure backups).

The Happie Co also runs a **hosted Mailie** at [mailie.thehappie.co](https://mailie.thehappie.co),
built on this server. Its web app, which adds reading and writing mail in the browser, and its
operations live in a private repository.

## Development

```sh
make check          # gofmt, vet, layout and the unit tests with -race; offline, no Node
make it             # the integration tier: Dovecot and Mailpit in Docker
make web-check      # the console's core: typecheck and tests (needs Node)
make public-source  # the public source snapshot, as it is published
make image          # the self-hosting image, mailie:local (Docker)
```

[`docs/architecture.md`](docs/architecture.md) explains the layout and the rules that keep mail
safe; [`CONTRIBUTING.md`](CONTRIBUTING.md) says how to send a change.

## License

Apache License 2.0: see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE). Copyright 2026 The Happie Co OÜ.

Mailie and the Mailie logo are trademarks of The Happie Co OÜ. The Apache License 2.0 does not grant
permission to use them (section 6) beyond describing where the software comes from; a modified version
distributed to others must not use the Mailie name or logo in a way that suggests it is the official
project.

Mailie is not affiliated with Google, Microsoft, Apple or any other mail provider.
