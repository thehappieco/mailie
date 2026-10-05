# Mailie

Mailie is a mail server for your own tools: one Go binary that connects several mailboxes at once
(Gmail and Microsoft 365 over OAuth2, iCloud Mail and any IMAP/SMTP server with a password) and
gives them a single **REST API** and an **MCP server**, so a script or an AI assistant can search,
read, act on and send mail with a key you control.

- **A local index, not a copy of your mail.** With your consent the server syncs each mailbox's
  metadata (senders, recipients, subjects, dates, sizes, folders, flags) into SQLite, with
  full-text search over subject, sender and recipients. Message bodies, attachments and originals
  are fetched from the mail server when someone asks for them and are never stored.
- **New mail in real time.** IMAP IDLE feeds an event journal, served as Server-Sent Events, a
  long poll and the MCP tool `wait_for_new_mail`.
- **Actions on the real mailbox.** Mark read or unread, star, archive, move and trash, on the mail
  server itself, only when the mailbox's owner has allowed it. Nothing is ever deleted for good,
  and reading a message never marks it as read.
- **Sending.** Plain-text messages, replies and forwards with attachments, through each mailbox's
  own SMTP server. Every send needs an explicit `confirm: true`, an idempotency key is reserved
  before the server is dialed, and a send whose outcome is unknown is never retried.
- **One service, two transports.** REST and MCP are thin adapters over the same service layer, so
  the same rules decide who sees what, whichever way a tool comes in.
- **The server console.** A small web console where the people you invite connect their own
  mailboxes, decide what the server may do with them, create API keys for their tools and see
  what the index holds.
- **Small to run.** No cgo: one static binary, one SQLite file, one data directory.

## Status

Mailie is under active development and has no stable release yet; the REST API and the MCP tools
may still change.

| Works today | Not yet |
|---|---|
| Gmail, Microsoft 365 and Outlook.com (OAuth2), iCloud Mail, generic IMAP | Drafts kept on the server |
| Sync with per-person consent, search, reading on demand | Search in message bodies |
| Read/unread, star, archive, move, trash, undo | Composing HTML |
| Sending over REST, with attachments, replies and forwards | MCP tools that send |
| MCP over Streamable HTTP and stdio, with read and action tools | OAuth for MCP clients (claude.ai web connectors) |
| The server console, invitations, personal API keys | Webhooks |
| Encrypted database backups to S3 | |
| A Docker image, Compose and a hardened systemd unit for self-hosting | |

## Quick start

You need Go 1.27, and Node 24 to build the console, or only Docker for the image
([`docs/self-hosting.md`](docs/self-hosting.md)). `make help` lists every target.

```sh
git clone https://github.com/thehappieco/mailie.git && cd mailie
cp .env.example .env
openssl rand -hex 32            # paste it into MAIL_CREDENTIAL_KEY_HEX in .env
make build                      # bin/mailserver
```

`MAIL_CREDENTIAL_KEY_HEX` encrypts every stored refresh token and password. The server refuses to
start without it. **Keep a copy with your other secrets: losing it means authorizing every mailbox
again.**

### With the console

Serve the built console from the daemon, at `http://localhost:8080`:

```sh
make web-install web-build      # the open console, into web/dist
cat >> .env <<'EOF'
MAIL_WEB_DIR=web/dist
MAIL_PUBLIC_URL=http://localhost:8080
EOF

# The first person, always an owner. Like every --bootstrap command, it opens the database
# directly and refuses to run while the daemon is up.
./bin/mailserver user invite --bootstrap --email you@example.com

./bin/mailserver serve          # 127.0.0.1:8080
```

Open the invite link the command printed, using `localhost` (not `127.0.0.1`), choose a name and a
password, and connect your mailboxes. iCloud and IMAP mailboxes need nothing more; Gmail and
Microsoft need the OAuth clients [below](#mailboxes-and-providers). To develop the console instead,
see [`docs/console.md`](docs/console.md#running-the-console-locally).

### With an admin key and the command line

```sh
./bin/mailserver apikey create --bootstrap --scope admin --name cli
export MAIL_ADMIN_KEY=<the key it printed>
./bin/mailserver serve &

./bin/mailserver account add --email you@icloud.com --password <app-specific password>
./bin/mailserver account list
./bin/mailserver account sync <account id> on      # a mailbox added this way syncs only when you say so
curl -H "Authorization: Bearer $MAIL_ADMIN_KEY" 'http://127.0.0.1:8080/v1/messages?q=invoice'
```

The command-line tools are REST clients of the running daemon and authenticate with
`MAIL_ADMIN_KEY`. `account add` for Gmail or Microsoft opens the provider's consent page (with
`--paste` on a machine without a browser); this needs the OAuth clients below.

## Mailboxes and providers

The OAuth registrations identify **your server**, not a mailbox: one registration serves every
account of that provider, and each person consents for their own mailbox in the browser.

There are two kinds of client, and a server can have both; each account remembers which one
issued its grant and always refreshes with it:

- **Installed clients** (`MAIL_GOOGLE_CLIENT_ID`, `MAIL_MICROSOFT_CLIENT_ID`) redirect to a
  listener the daemon opens on loopback for each authorization (`http://127.0.0.1:<port>` for
  Google, `http://localhost:<port>` for Microsoft). The command line uses them, and so does a
  console whose `MAIL_PUBLIC_URL` is on `localhost`: the browser must be on the daemon's machine.
- **Web clients** (`MAIL_GOOGLE_WEB_CLIENT_ID`/`_SECRET`, `MAIL_MICROSOFT_WEB_CLIENT_ID`/`_SECRET`)
  redirect to `<MAIL_PUBLIC_URL>/oauth/return`. A console reached from anywhere else needs them;
  a console that is not on loopback is never offered the loopback flow.

### Google

1. In the [Google Cloud Console](https://console.cloud.google.com/), create a project and
   configure the **OAuth consent screen** (type *External*).
2. Add the scope `https://mail.google.com/`. Google lists it as *restricted*; it is the only scope
   that IMAP and SMTP accept.
3. Set the publishing status to **In production**, even unverified. In *Testing*, every refresh
   token expires after 7 days. An unverified app shows Google's "unverified app" screen and is
   limited to 100 users over the project's life, which is plenty for yourself and your team.
4. **Credentials → Create credentials → OAuth client ID:**
   - **Desktop app** for the installed client. There is no redirect to register: Google accepts
     the loopback address. Put the id (and the secret Google shows, which protects nothing for a
     desktop client) in `MAIL_GOOGLE_CLIENT_ID` and `MAIL_GOOGLE_CLIENT_SECRET`.
   - **Web application** for the console. Authorized redirect URI: exactly
     `<MAIL_PUBLIC_URL>/oauth/return`, port included (for example
     `http://localhost:8080/oauth/return`, or `https://mail.example.org/oauth/return`). No
     JavaScript origins: the daemon exchanges the code, never the browser. Put the id and secret
     in `MAIL_GOOGLE_WEB_CLIENT_ID` and `MAIL_GOOGLE_WEB_CLIENT_SECRET`.

Google allows 15 simultaneous IMAP connections per account across all of its clients (Mailie uses
at most three), and changing the account's password revokes the refresh token.

### Microsoft 365 and Outlook.com

1. In the [Entra admin center](https://entra.microsoft.com/), **App registrations → New
   registration**, for "Accounts in any organizational directory and personal Microsoft
   accounts". `MAIL_MICROSOFT_TENANT` stays `common`.
2. **API permissions → Add → APIs my organization uses → Office 365 Exchange Online → Delegated**:
   `IMAP.AccessAsUser.All` and `SMTP.Send`. If the tenant restricts user consent, an administrator
   grants them. The daemon also asks for `offline_access`.
3. The installed client: **Authentication → Add a platform → Mobile and desktop applications**,
   redirect `http://localhost` (Entra ignores the port; `127.0.0.1` and `[::1]` are not accepted),
   and **Allow public client flows: Yes**. No secret. Its id goes in `MAIL_MICROSOFT_CLIENT_ID`.
4. The web client, as a **separate registration**: **Add a platform → Web**, redirect
   `<MAIL_PUBLIC_URL>/oauth/return` (for local use `http://localhost/oauth/return`, which covers
   any port), **Allow public client flows: No**, and a client secret under **Certificates &
   secrets**. Never `/oauth/callback`, the loopback path. Id and secret go in
   `MAIL_MICROSOFT_WEB_CLIENT_ID` and `MAIL_MICROSOFT_WEB_CLIENT_SECRET`. Note the secret's expiry
   date: an expired secret makes every refresh fail with `invalid_client`, which the daemon reports
   as a configuration error, not as a revoked grant.

Microsoft refuses passwords for IMAP, so Microsoft mailboxes connect with OAuth only. In a work
tenant, each mailbox needs IMAP and, for sending, SMTP AUTH enabled:

```powershell
Set-CASMailbox -Identity user@contoso.com -ImapEnabled $true -SmtpClientAuthenticationDisabled $false
```

Security defaults and Conditional Access must allow IMAP and SMTP with modern authentication. A
personal Outlook.com account that answers `BAD User is authenticated but not connected` needs
"Let devices and apps use IMAP" turned on in its settings. The device-code flow
(`account add --device`, `MAIL_MICROSOFT_DEVICE_CODE=true`) exists for machines without a browser,
but Entra security defaults block it.

### iCloud Mail and other IMAP servers

Apple offers no OAuth to third-party apps. An iCloud mailbox connects with an **app-specific
password** (it needs two-factor authentication: account.apple.com → Sign-In and Security →
App-Specific Passwords); the daemon knows Apple's servers. A custom iCloud+ domain signs in with the
account's `@icloud.com` address as the user name:

```sh
mailserver account add --email you@icloud.com --password <app-specific password>
mailserver account add --email you@yourdomain.com --provider icloud \
  --login-user you@icloud.com --password <app-specific password>
```

Any other IMAP server takes its own host, port and password (`--imap-host`, `--smtp-host`,
`--smtp-tls`, …). Every password account logs in once before it is saved. IMAP and SMTP always
use TLS. A host that resolves to a loopback, private, link-local or CGNAT address is refused when
the daemon dials it unless `MAIL_ACCOUNT_ALLOW_PRIVATE=true`, so a host typed into the console
cannot reach your internal network.

## The console

The console is a static build (`web/`, Vue 3) that the daemon serves on the same origin as the API
when `MAIL_WEB_DIR` points at it. Each person signs in to their own account and has four sections:
**Mailboxes** (connect, authorize, follow sync), **API keys & MCP**, **Storage** (what their
mailboxes take up in the index) and **Account** (name, password, sessions, and the permissions for
sync and for actions). There is no mail to read or write in it: a tool does that, with a key.

- Sessions are bearer tokens, never cookies, valid for 14 days.
- People are invited (`mailserver user invite --email X [--role owner|member]`), never sign up on
  their own. An `owner` also sees the mailboxes nobody owns, which the command line creates; a
  `member` sees only their own. Another person's mailbox does not exist for you.
- Nothing from a person's mailboxes is stored until they turn sync on in the console, and turning
  it off deletes their index. Actions on their messages are a second, separate permission.
- `mailserver user disable --email X` ends a person's sessions and revokes their keys;
  `mailserver user delete --email X` deletes them with their mailboxes, credentials, index,
  sessions and keys.
- A forgotten password is reset by the operator with the daemon stopped:
  `mailserver user password --bootstrap --email X` asks for the new one twice (or reads one line
  piped in) and ends every session that person has. No route sets a password.

[`docs/console.md`](docs/console.md) covers the console, its REST API, consent, events and the
OAuth flows in detail.

## The API

Every route is under `/v1` and takes `Authorization: Bearer <key or session>`. Errors are
`{"code", "message"}` with one of seven codes. A few examples:

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

### Keys and scopes

| Scope | Allows |
|---|---|
| `read` | listing, searching, reading bodies, downloading attachments and originals, waiting for new mail. It never marks anything as read. |
| `write` | the above, plus flags, moves, archive and trash. In a person's mailbox only that person acts, with their session or a key they created, and only after allowing actions in the console. |
| `send` | the above, plus sending. A person's mailbox sends only for that person, signed in, after they allowed sending. |
| `admin` | the above, plus mailboxes, keys and people. |

There are two kinds of key:

- **Instance keys**, for the operator: `mailserver apikey create --scope SCOPE --name NAME
  [--accounts IDS] [--days N]`, at most a year, optionally restricted to some mailboxes. Over REST
  they see every mailbox, but never change or send from a person's mailbox. Over MCP they reach
  only the mailboxes nobody owns.

  `apikey create`, `apikey list` and `apikey revoke PREFIX` call the daemon's `/v1/apikeys` routes,
  which exist only while it runs with `MAIL_ADMIN_API=true` (off by default; otherwise the daemon
  answers 404). Without them, `apikey create --bootstrap` issues a key straight into the database,
  with the daemon stopped, as for the first key above; listing and revoking need the routes.
- **Personal keys**, which each person creates in the console for their own tools: `read` or
  `write`, for all or some of their mailboxes, for 30, 90 or 365 days, at most 20 alive. They are
  the only keys that act as a person, and the only way a tool reaches a person's mailbox.

Keys are stored only as Argon2id hashes and shown once. Revoked keys stay listed, because they
answer "what could have read this mailbox". A key restricted to mailboxes that have all been
removed is revoked rather than widened to every mailbox.

## MCP

The daemon is an MCP server: Streamable HTTP at `/mcp` and stdio. Tools to read (`list_accounts`,
`list_folders`, `search_messages`, `get_message`, `get_attachment`, `wait_for_new_mail`) and to act
(`mark_read`, `flag_message`, `move_message`, `trash_message`), and `mail://` resources. MCP takes
API keys only, never a console session; a person creates theirs in the console.

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
with the first owner, OAuth clients, backups, upgrades and a forgotten password. In short:

- The daemon listens on `127.0.0.1:8080` and speaks plain HTTP. To reach it from elsewhere, put a
  reverse proxy that terminates TLS in front, set `MAIL_TRUSTED_PROXIES` to the proxy's address
  (otherwise every client shares one rate limit), `MAIL_PUBLIC_URL` to the public `https://`
  origin, and `MAIL_ENV=prod`. Turn response buffering off for `/v1/events` and `/mcp`, which
  stream.
- `/metrics` (Prometheus) is served on the main listener unless `MAIL_METRICS_ADDR` gives it one
  of its own; keep it off the public proxy.
- One daemon per data directory: a lock refuses a second one. `mailserver migrate` applies schema
  migrations; the daemon also applies them when it starts.
- To rotate `MAIL_CREDENTIAL_KEY_HEX`, set the new key with a new `MAIL_CREDENTIAL_KEY_ID`, keep the
  old one in `MAIL_CREDENTIAL_PREVIOUS_KEYS` (`<id>:<hex>`), and run
  `mailserver rewrap-credentials` with the daemon stopped. The daemon then no longer needs the old
  key, but do not destroy it: `rewrap-credentials` re-seals the live database only, so every backup
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
  AES-256-GCM under `MAIL_CREDENTIAL_KEY_HEX` and bound to its account and field; the index of
  metadata of synced mailboxes; Argon2id hashes of passwords and keys; SHA-256 hashes of session
  tokens. Message bodies and attachments are never stored: while one is being served it sits in a
  `0600` temporary file that is removed when the response ends.
- **Who can read it.** Whoever has both the database and `MAIL_CREDENTIAL_KEY_HEX` can reach the
  mailboxes; whoever runs the server can read the index. The key lives in the environment, which
  protects copies and backups of the database, not against code running as the daemon's user.
- **Who sees what.** For other people, owners included, a person's mailbox does not exist: it is
  `not_found`, never `forbidden`. Instance keys belong to the operator: over REST they see every
  mailbox but never change a person's messages or send from a person's mailbox, and over MCP they
  reach only the mailboxes nobody owns. Changing a person's mailbox or sending from it needs that
  person's own consent, checked again before every command that changes the mailbox and before
  every connection to the submission server.
- **No local exception.** A process on the same machine still needs a key. Rate limits apply per
  address and to failed attempts per key prefix; failures never lock out a whole address.
- **Logs** carry ids, never subjects, addresses, bodies, tokens or passwords.

Report vulnerabilities as described in [`SECURITY.md`](SECURITY.md).

## Editions

This repository is the **self-hosted edition**: the server, the command line and the server
console, under Apache-2.0. Running it needs no account or subscription, and it talks to nobody but
your mail providers (and AWS, if you configure backups).

The Happie Co also runs a **hosted Mailie** at [mailie.thehappie.co](https://mailie.thehappie.co),
built on this server. Its web app, which adds reading and writing mail in the browser, and its
operations live in a private repository.

## Development

```sh
make check          # gofmt, vet, layout and the unit tests with -race; offline, no Node
make it             # the integration tier: Dovecot and Mailpit in Docker
make web-check      # the console: typecheck, tests and build (needs Node)
make public-source  # the public source snapshot, as it is published
make image         # the self-hosting image, mailie:local (Docker)
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
