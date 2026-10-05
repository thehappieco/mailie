# Mailie

A Go server that exposes REST and MCP over several mail accounts (Gmail, Microsoft 365, iCloud,
generic IMAP). A local SQLite index; bodies and attachments on demand; IMAP IDLE for new mail;
sending over SMTP with XOAUTH2. `docs/architecture.md` is the long form of this file.

## Layout

- `cmd/mailserver` — one binary: `serve`, `account`, `apikey`, `user`, `migrate`,
  `rewrap-credentials`, `backup`, `mcp connect|install`. The CLI is a REST client of the daemon; only
  `apikey create --bootstrap`, `user invite|disable|delete|password --bootstrap`, `migrate` and
  `rewrap-credentials` open the database directly, and they refuse to run while the daemon is up.
  The one exception to the lock is `backup`: it reads the database with the daemon up, through a
  `mode=ro` connection that writes nothing, and takes no lock. `backup` and `backup restore` load
  their own configuration (`config.LoadBackup`/`LoadRestore`) before `config.Load`; a restore runs
  wherever the backups may be decrypted, with none of the daemon's variables (`make backup-tool`
  builds a Linux binary for it). `mcp connect` and `mcp install` run beside an MCP client, on any
  machine, before `.env` and `config.Load`: they need no `MAIL_*` variable and touch no database.
  `connect` is a stdio MCP server relaying to a server's `/mcp` with the key from `MAILIE_API_KEY`;
  `install` writes Claude Desktop's or Cursor's configuration (atomic, 0600, one `.bak-mailie`, empty
  when there was no file, never from `--uninstall`; no symbolic link below the home on the way) after
  checking the key, and for Claude Code only prints the command. The key is never an argument.
- `internal/app` — the daemon's assembly (`app.Run`), which `serve` calls and another binary may
  too; `Options.Extensions` mounts routes before the console's catch-all, refused at start if one
  names a host, duplicates or would take a request from a core route. An extension signs people in
  through a provider with `Service.SignInExternal`/`PinIdentityKey`; `Options.ExternalSignInOnly`
  (never set by `serve`) has the service refuse every password and invitation route.
- `internal/config` — env only (`MAIL_*`) plus `.env`; `Load()` returns every error at once;
  `String()` redacts secrets.
- `internal/obs` — `log/slog` with redaction of addresses and credentials, Prometheus metrics.
- `internal/lockfile` — the data directory's lock: two daemons on one database corrupt the index.
- `internal/store` — SQLite (modernc, no cgo): a writer pool with `_txlock=immediate` and
  `MaxOpenConns(1)`, a reader pool with `query_only`, embedded `.sql` migrations plus
  `PRAGMA user_version`, FTS5.
- `internal/secrets` — a versioned AES-256-GCM envelope (`v1||keyid||nonce||ct||tag`) with AAD
  binding account and field; rotation by key id.
- `internal/auth` — API keys `prefix.secret`, Argon2id PHC, scopes `read < write < send < admin`,
  restriction to accounts, expiry, revocation. Also the console's people: users (Argon2id password
  on the server, instance roles `owner`/`member`), sessions (a 43-character opaque token stored as
  SHA-256, 14 days at most, never extended) and single-use invites, to the instance or into a team.
  External identities (issuer + subject, linked only with a verified address) and their key pins
  (insert only, deleted only with the person, or by the hourly sweep when no sign-in linked them);
  a person who signs in that way has an empty password hash, which no password check accepts.
  `authtest` creates cheap users and keys for tests.
- `internal/ratelimit` — token buckets per address (IPv6 per /64) and per key prefix. An
  authentication failure is reserved on the prefix and given back if the credential proves good; it
  is never charged to the whole address, which would refuse valid credentials behind the same NAT.
- `internal/backup` — the snapshot (`VACUUM INTO` from a `mode=ro` connection), `integrity_check`,
  the `.mlbk` format (AES-256-GCM STREAM chunks under a KMS data key, the header as AAD) and restore.
  KMS and S3 sit behind small interfaces; `aws.go` is the only file that talks to the SDK, and
  `backuptest` has the fakes. See `docs/backup.md`.
- `internal/events` — the journal (`events`), written in the same transaction as the change, and
  the live fan-out bus.
- `internal/xoauth2` — the XOAUTH2 SASL mechanism (client and server); go-sasl does not have it.
- `internal/netguard` — refuses loopback, private, link-local and CGNAT in `net.Dialer.Control`, on
  the address already resolved; it is what stops an account host typed into the console from
  becoming SSRF.
- `internal/provider` — the `Mailbox`/`Session`/`Sender` interfaces plus a `Profile` per provider;
  `providertest.FakeMailbox` (a stateful fake) is the seam of the sync tests.
  `internal/provider/imap` is the only implementation and the only package that imports
  `imapclient` and `go-mail`.
- `internal/mime` — a lenient parser over go-message; parts ↔ IMAP sections.
- `internal/account` — accounts, their encrypted credentials, OAuth flows and the registry of
  token sources.
- `internal/workspace` — workspaces (personal, team, the one operator workspace), members and
  per-mailbox grants (`read`, `act`, `send`, `manage`), the protections each write keeps (last
  owner, linker, last manager) and the workspace `Source` (local; the platform's is a stub). See
  `docs/workspaces.md`.
- `internal/sync` — one worker per account, three sessions of fixed role (`idle`, `sync`,
  `interactive`).
- `internal/service` — the use cases and all authorization, access to mailboxes included; a
  concrete struct, not an interface.
- `internal/api` — REST `/v1/...`, SSE and the long poll. `internal/mcp` — tools, resources and the
  transports (stateful Streamable HTTP at `/mcp` unless `MAIL_MCP_HTTP=false`, stdio); one
  `sdk.Server` per session, for the key that opened it, with the same tool definitions for the whole
  process (the SDK's `SchemaCache` keeps each schema by pointer, forever), and an event store of its
  own bounded in bytes and age. Both are thin adapters over `internal/service`
  (`AuthenticateTool` decides what a tool reaches). See `docs/mcp.md`.
- `internal/mcpbridge` — the relay behind `mcp connect`: the go-sdk's stdio transport towards the
  client and its Streamable HTTP client towards the server, copying JSON-RPC messages unread. It adds
  what that client does only inside the SDK's own session (the `MCP-Protocol-Version` header, the
  standalone GET stream), opens a session the server ended again with the client's own `initialize`
  and subscriptions, never follows a redirect, and sends the key only to the endpoint. `Check` opens
  and closes a session with a key. See `docs/mcp.md`, "Connect a client".
- `internal/webui` — serves the built console from `MAIL_WEB_DIR` (via `os.Root`), mounted last;
  a JSON 404 for unknown `/v1/`, `/mcp` and `/.well-known/` paths. It is also a transport for
  depguard.
- `web/` — the console: Vue 3 + TS + Vite, with no router, Pinia or UI library. A Node project of
  its own: `go.mod` ignores it, `make check` does not touch it. `web/src` is the **core** every
  edition of the console shares, plus the **open edition** (`web/src/open`), which a self-hosted
  server serves. Another edition (the hosted service's app, private) compiles `web/src` from
  source; nothing under `web/src` imports from outside it, and the open console names nothing of
  the hosted service (`web/test/editions.spec.ts`, `web/test/hosted.ts`). See `docs/console.md`.
- `deploy/` — the self-hosting packaging (`docs/self-hosting.md`): `Dockerfile` (Node builds the
  open console, Go a static binary, both onto distroless `static-debian13:nonroot`; the build
  context is the root, where `.dockerignore` is an allowlist), `compose.yaml` (read-only, no
  capabilities, the port on loopback, a named volume, a fixed subnet whose gateway is the
  documented `MAIL_TRUSTED_PROXIES`) and example systemd units (`mailie.service`,
  hardened; `mailie-backup.service` and its timer). Base images are pinned by digest: a Go or Node
  bump (go.mod's toolchain, CI's `GO_VERSION`) moves the Dockerfile's stage with it. `make image`
  builds `mailie:local`.
- `scripts/export-public.py` — `make public-source`: the public source as an allowlist of this
  checkout, without history: exactly what Git tracks there, refusing a file Git ignores or does not
  track yet, symlinks, environment files, local secrets and forbidden strings; the one local file it
  skips, unread, is `deploy/.env` (the Compose stack's environment file, while untracked). CI builds
  and tests the export on its own (`distribution` job).
- `commercial/` — a development checkout of the hosted service nests its own, private repository
  here. This repository ignores it (`.gitignore`, `go.mod`'s `ignore`, the Makefile's `fmt` and
  `lint-layout`) and the exporter never exports it; nothing here may depend on it. Local,
  untracked instructions go in `CLAUDE.local.md` (ignored too).

## How to run

```sh
cp .env.example .env            # fill MAIL_CREDENTIAL_KEY_HEX (openssl rand -hex 32)
make build
./bin/mailserver apikey create --bootstrap --scope admin --name cli   # no daemon, like user invite --bootstrap
export MAIL_ADMIN_KEY=<the printed key>
./bin/mailserver serve          # 127.0.0.1:8080, MCP at /mcp, stdio with --mcp-stdio
```

Console: `MAIL_PUBLIC_URL=http://localhost:5174` in `.env`,
`./bin/mailserver user invite --bootstrap --role owner --email you@example.com` (with the daemon stopped), the
daemon, and `make web-install && make web-dev`. Open the invite link with `localhost`, never
`127.0.0.1`. Details, and registering the OAuth clients, in `README.md` and `docs/console.md`.

## How to test

- `make check` = fmt + vet + lint-layout + `go test -race ./...`. All offline: an in-process IMAP
  server, a fake `go-smtp` server, `httptest` for OAuth, SQLite in `t.TempDir()`, MCP transports in
  memory. `make lint` runs golangci-lint with the integration tag.
- `make it` starts `it/compose.yml` (Dovecot 2.4.5, Mailpit) and runs `go test -tags integration`.
  It is the only place CONDSTORE, SPECIAL-USE and a real XOAUTH2 run; CI runs it, it is not
  optional.
- Console: `make web-check` (typecheck, vitest, build), outside `make check`; CI runs it in a job of
  its own on Node 24. The contract fixtures in `internal/api/testdata/contract/` are written by the
  real handlers (`go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`) and
  read by `web/test/contract.spec.ts`; never edit them by hand or create them from the web side.
- `make public-source` exports the public snapshot; run it before a pull request.
- `make image` builds the self-hosting image (Docker, outside `make check`); CI's `image` job builds
  it through `deploy/compose.yaml`, starts it with a throwaway key and checks `/v1/healthz`, the
  console and a clean stop.
- Tests that connect to the in-process IMAP server (on `127.0.0.1`) turn `AllowPrivate` on, as the
  integration tier does; the ones that prove the private-host refusal leave it off.

## Rules

- **No cgo.** `CGO_ENABLED=0` in the Makefile and in CI for every build. The race detector is the
  one exception: on Linux it needs cgo, so the race runs alone turn it on there (`test-race`,
  `cover`, CI's `go test -race`), and nothing they build ships. `modernc.org/sqlite` and
  `modernc.org/libc` move together or not at all.
- **Zero duplicated logic between REST and MCP.** Handlers and tools are thin adapters over
  `internal/service`; all authorization lives there. `depguard` stops a transport from importing
  `store`, `sync`, `provider`, `account` or `workspace` (access is decided in the service). If a rule
  appears on both sides, it is in the wrong place.
- **Sending needs confirmation.** `confirm: true` is required on every send; the two-step
  draft → send path is the planned alternative for MCP. Never relax this in a test or behind a
  flag. Idempotency is reserved in the database **before** dialing SMTP, and a connection that
  drops or goes silent after the body started becomes `unknown`, which is never retried
  automatically (Exchange delivers both copies). A 4xx/5xx answer at the end of `DATA` is a refusal
  (RFC 5321): `failed` (`provider.ErrAfterData`), also never retried. Who may send is asked again by
  `Outgoing.BeforeDial`, under the account's lock, before every connection. An SMTP AUTH refusal
  never changes the account's state: if the grant died, IMAP says so.
- **Never `ENABLE QRESYNC` nor ask for `X-GM-*` items.** go-imap v2 drops the connection on any
  response it does not know. The sync track (`condstore` or `uidpoll`) is chosen by `Caps()` at
  run time, never assumed from the provider.
- **Always UID.** IMAP commands take `imap.UIDSet`; the dynamic type of the `NumSet` decides between
  UID and sequence number, and getting it wrong deletes the wrong message.
- **Unilateral data handlers only write to a channel.** They run on the client's read goroutine;
  issuing a command there deadlocks the connection.
- **Connection budget per account: 3.** One `idle` session (INBOX only), one `sync`, one
  `interactive`. Gmail allows 15 in total for all of a user's apps.
- **No reader bound to a connection crosses the provider boundary.** Parts and attachments are
  written to a temporary file while the session is held, and only then handed over.
- **Sent copy:** never `APPEND` for Gmail and Microsoft (they file it themselves); only for generic
  IMAP with `save_sent_copy=1`.
- **RFC ids without brackets in the database.** The `<>` are added in one place, the go-mail
  builder. `msg_id` is the local FK, `message_id` is the header: never mix them.
- **One tool, one annotation class.** `trash_message` is separate from `move_message`; read tools
  never mutate (`get_message` does not mark a message read).
- **Events.** Journal and state change in the same transaction; publish only after the commit.
  Replay always comes from the `events` table — there is no history in memory.
- **Secrets.** Never in plain text in the database, in logs (`obs` redacts) or in URLs. TLS is
  mandatory for IMAP/SMTP; `AllowInsecureAuth` is set only by tests.
- **Console session = bearer, never a cookie.** The token goes only in `Authorization`; sessions and
  API keys are told apart by their shape (a key has a dot). Person routes (`/v1/auth/*`) refuse API
  keys.
- **Access to a mailbox is decided in `internal/service`.** A person sees one as an active member
  of its workspace holding a grant on it, and each use needs its flag; an instance key reaches only
  the operator workspace's. Another workspace's mailbox, or one the caller holds nothing on, is
  `not_found`, never `forbidden`; listing filters in SQL with the same rule as fetching one. No role
  reads mail by being one: owners and admins grant, and `read`, `act` and `send` pass only from a
  holder. The linker's consent syncs the mailbox (`accounts.owner_user_id` means "linked by").
- **An OAuth flow belongs to whoever started it.** `oauth_pending.owner_user_id` is checked in the
  same `DELETE` that consumes the row, **before** the code is exchanged; another owner's is
  `not_found` and the row stays. The web flow's redirect is `MAIL_PUBLIC_URL + /oauth/return` (a
  console route), never derived from `Host`; a console that is not on loopback is never offered
  `loopback`. Refresh uses the client that issued the token (`accounts.oauth_client`);
  `invalid_client` is a configuration error, not a dead grant.
- **A private account host is refused at dial time**, via `netguard.Control` on the IMAP and SMTP
  dialers, unless `MAIL_ACCOUNT_ALLOW_PRIVATE`. A password account logs in once before it is saved.
- **The console never shows the server's `message`**: it translates the `code`. The `state_reason`
  of a failed consent is a fixed English text (`account.Reason*`), never the provider's.
- **Consent.** Nothing of a person's messages is stored before they consent to sync, and
  withdrawing deletes their index in the same transaction. Actions and sending have their own
  consents, checked again right before the mail server is touched. The revisions come from
  `MAIL_CONSENT_VERSION_*`, whose defaults are the open console's texts (`web/src/open/versions.ts`).
- **Errors.** Sentinels `errors.New("pkg: ...")`, always `errors.Is`/`errors.As`. The API answers
  `{code, message}` with seven fixed codes and never says which credential failed.
- **Backup: the host only encrypts and uploads.** It generates data keys with the context exactly
  `{service: mailie, env: MAIL_ENV, purpose: db-backup, ref: <object key>}`
  (`backup.EncryptionContext`) and creates new objects under `db/` (`If-None-Match: *`, SSE-S3,
  never SSE-KMS); decrypting, reading, listing and deleting belong to another principal. Plaintext
  only in the run's 0700 directory. Never `query_only` on the snapshot connection (it refuses
  `VACUUM INTO`); `mode=ro` is the guarantee. AWS credentials only from the SDK's default chain,
  never from `MAIL_*`. The restore trusts only the key it is given (`--kms-key-arn`), never the ARN
  in the header, which nothing authenticates.
- **Config only through env.** No YAML, no TOML. Logs through `slog` with fields, never
  `fmt.Println`.
- Everything in this repository is written in English: code, comments, logs, errors, README,
  `docs/`, commits and PRs. UI strings stay in the console's multilingual catalogs. Commits in the
  imperative mood, no prefix. Test names are sentences that state a guarantee
  (`TestAReadKeyCannotSend`), not `TestFuncName`.
