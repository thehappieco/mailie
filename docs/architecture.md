# Architecture

Mailie is one Go binary (`cmd/mailserver`) over one SQLite database, plus a web console built
separately (`web/`). This page is the map for contributors: where things live, and the rules that
are easy to break and expensive when broken. Most of them exist because the alternative loses mail,
sends it twice or leaks a credential.

## Layout

| Path | What it holds |
|---|---|
| `cmd/mailserver` | The binary: `serve`, `account`, `apikey`, `user`, `migrate`, `rewrap-credentials`, `backup`, `mcp connect\|install`. The command line is a REST client of the daemon. Only `apikey create --bootstrap`, `user invite\|disable\|delete\|password --bootstrap`, `migrate` and `rewrap-credentials` open the database directly, and they refuse to run while the daemon holds its lock. `backup` is the one exception to the lock: it reads the database beside a running daemon through a `mode=ro` connection that writes nothing. `backup` and `backup restore` load their own configuration (`config.LoadBackup`/`LoadRestore`) before `config.Load`, so a restore runs where none of the daemon's variables exist. `mcp connect` and `mcp install` run beside an MCP client on any machine, before `.env` and `config.Load`: they need no `MAIL_*` variable and open no database ([`mcp.md`](mcp.md#connect-a-client)). |
| `internal/config` | Environment only (`MAIL_*`, optionally seeded from `.env`). `Load()` returns every error at once; `String()` redacts secrets. |
| `internal/obs` | `log/slog` with redaction of addresses and credentials; Prometheus metrics. |
| `internal/lockfile` | The data directory's lock: two daemons on one database would corrupt the index. |
| `internal/store` | SQLite through `modernc.org/sqlite` (no cgo): a writer pool with `_txlock=immediate` and `MaxOpenConns(1)`, a reader pool with `query_only`, embedded `.sql` migrations tracked by `PRAGMA user_version`, FTS5. |
| `internal/secrets` | A versioned AES-256-GCM envelope (`v1‖keyid‖nonce‖ct‖tag`) whose additional data binds the account and the field; rotation by key id. |
| `internal/auth` | API keys `prefix.secret` hashed with Argon2id (PHC strings), scopes `read < write < send < admin`, restriction to accounts, expiry, revocation. Also the console's people: users (Argon2id passwords, roles `owner`/`member`), sessions (a 43-character opaque token stored as SHA-256, 14 days) and single-use invitations. `authtest` mints cheap users and keys for tests. |
| `internal/ratelimit` | Token buckets per address (IPv6 per /64) and per key prefix. A failed authentication is reserved on the prefix and given back if the credential proves good; it is never charged to the whole address, which would lock out good credentials behind the same NAT. |
| `internal/backup` | The snapshot (`VACUUM INTO` from a `mode=ro` connection), `integrity_check`, the `.mlbk` format (AES-256-GCM STREAM chunks under a KMS data key, the header as additional data) and restore. KMS and S3 sit behind small interfaces; `aws.go` is the only file that talks to the AWS SDK, and `backuptest` has the fakes. See [`backup.md`](backup.md). |
| `internal/events` | The journal (`events`), written in the same transaction as the change it records, and the live fan-out bus. |
| `internal/xoauth2` | The XOAUTH2 SASL mechanism, client and server; go-sasl does not have it. |
| `internal/netguard` | Refuses loopback, private, link-local and CGNAT addresses in `net.Dialer.Control`, on the address already resolved: what stops a host typed into the console from becoming SSRF. |
| `internal/provider` | The `Mailbox`/`Session`/`Sender` interfaces and a `Profile` per provider. `providertest.FakeMailbox`, a stateful fake, is the seam of the sync tests. `internal/provider/imap` is the only implementation and the only package that imports `imapclient` and `go-mail`. |
| `internal/mime` | A lenient parser over go-message; parts ↔ IMAP sections. |
| `internal/account` | Accounts, their credentials and OAuth flows, the registry of token sources. |
| `internal/sync` | One worker per account with three sessions of fixed role (`idle`, `sync`, `interactive`). |
| `internal/service` | The use cases and **all** authorization, account ownership included. A concrete struct, not an interface. |
| `internal/api` | REST under `/v1`, Server-Sent Events, the long poll. |
| `internal/mcp` | MCP tools, resources and transports (stateful Streamable HTTP at `/mcp`, stdio). One `sdk.Server` per session, built for the key that opened it, with the same tool definitions for the whole process (the SDK's `SchemaCache` keys each schema by pointer, forever), and an event store of its own bounded in bytes and age. See [`mcp.md`](mcp.md). |
| `internal/mcpbridge` | The relay behind `mcp connect`: the go-sdk's stdio transport towards the client and its Streamable HTTP client towards a server's `/mcp`, copying JSON-RPC messages without reading them. It sends the negotiated `MCP-Protocol-Version` and holds the standalone GET stream (which that client does only inside the SDK's own session), asking for it again with `Last-Event-ID`, through refusals, and in a new session when the server can no longer resume it or keeps holding it for a dead connection; it opens a session the server ended again with the client's own `initialize` and subscriptions, never follows a redirect, and sends the key only to the endpoint. |
| `internal/webui` | Serves the built console from `MAIL_WEB_DIR` through `os.Root`, mounted last; unknown `/v1/`, `/mcp` and `/.well-known/` paths answer a JSON 404. A transport, for the import rules below. |
| `it/` | The integration tier's services: Dovecot 2.4.5 and Mailpit. |
| `web/` | The console: Vue 3, TypeScript and Vite, without a router, Pinia or a UI library. A Node project of its own: `go.mod` ignores it and `make check` never touches it. See [`console.md`](console.md). |
| `deploy/` | The self-hosting packaging, described in [`self-hosting.md`](self-hosting.md): a `Dockerfile` (Node 24 builds the open console, Go a static binary, both onto distroless `static-debian13:nonroot`, base images pinned by digest; the build context is the root, where `.dockerignore` is an allowlist), a `compose.yaml` (read-only, no capabilities, the port on the host's loopback, the database on a named volume) and example systemd units: `mailie.service`, hardened, and `mailie-backup.service` with its timer. |
| `scripts/export-public.py` | `make public-source`: the public source as an allowlist of the checkout, without history: exactly what Git tracks there, refusing a file Git ignores or does not track yet, symlinks, environment files, local secrets and databases, and forbidden strings. CI builds and tests the export on its own. |

### The console's editions

`web/src` is a core that every edition of the console shares, plus the open edition in
`web/src/open`, which is what this repository builds and a self-hosted server serves. The hosted
service's app is another edition, in a private repository: it compiles `web/src` from source and
adds reading and writing mail. The core never imports from outside `web/src`, and the open console
names nothing of the hosted service; `web/test/editions.spec.ts` and the build enforce both.

A development checkout of the hosted service nests its private repository at `commercial/`. This
repository ignores that directory (`.gitignore`, `go.mod`'s `ignore`, the `Makefile`'s `fmt` and
`lint-layout`), and the exporter never exports it. Nothing here may depend on it.

## Rules

### Build

- **No cgo.** `CGO_ENABLED=0` in the Makefile and in CI for every build. The race detector is the
  one exception: on Linux it needs cgo, so the race runs alone turn it on there (`make test-race`,
  `make cover`, CI's `go test -race`), and nothing they build ships. `modernc.org/sqlite` and
  `modernc.org/libc` are upgraded together or not at all.
- **Configuration is environment only.** No YAML, no TOML. Logs go through `slog` with fields, never
  `fmt.Println`.
- **Errors** are sentinels (`errors.New("pkg: …")`) checked with `errors.Is`/`errors.As`. The API
  answers `{code, message}` with seven fixed codes and never says which credential check failed.

### Authorization lives in the service

- **No duplicated logic between REST and MCP.** Handlers and tools are thin adapters over
  `internal/service`, where every authorization decision is made. `depguard` (`.golangci.yml`)
  stops a transport (`internal/api`, `internal/mcp`, `internal/webui`) from importing `store`,
  `sync`, `provider` or `account`. If a rule appears on both sides, it is in the wrong place.
- **Ownership is decided in `internal/service`.** Another person's mailbox is `not_found`, never
  `forbidden`, and listing filters in SQL with the same rule as fetching one. An `owner` also sees
  the mailboxes nobody owns (created with an instance key); a `member` sees only their own.
- **A console session is a bearer token, never a cookie.** It travels only in `Authorization`;
  sessions and API keys are told apart by their shape (a key has a dot). Person routes
  (`/v1/auth/*`) refuse API keys.
- **An OAuth flow belongs to whoever started it.** `oauth_pending.owner_user_id` is checked in the
  same `DELETE` that consumes the row, **before** the code is exchanged; another owner's is
  `not_found` and the row stays. The web flow's redirect is `MAIL_PUBLIC_URL + /oauth/return`, never
  derived from `Host`, and a console that is not on loopback is never offered the loopback flow.
  Refreshing uses the client that issued the token (`accounts.oauth_client`); `invalid_client` is a
  configuration error, not a dead grant.
- **Private account hosts are refused when dialing**, through `netguard.Control` on the IMAP and SMTP
  dialers, unless `MAIL_ACCOUNT_ALLOW_PRIVATE`. A password account logs in once before it is saved.
  Tests that connect to the in-process IMAP server on `127.0.0.1` turn `AllowPrivate` on, as the
  integration tier does; the ones that prove the refusal leave it off.
- **The console never shows the server's `message`**: it translates the `code`. The `state_reason`
  of a failed consent is a fixed English text (`account.Reason*`), never the provider's.

### IMAP

- **Never `ENABLE QRESYNC`, never ask for `X-GM-*` items.** go-imap v2 drops the connection on any
  response it does not know. The sync track (`condstore` or `uidpoll`) is chosen from `Caps()` at
  run time, never assumed from the provider.
- **Always UID.** IMAP commands take an `imap.UIDSet`: the dynamic type of the `NumSet` decides
  between UIDs and sequence numbers, and getting it wrong changes the wrong message.
- **Unilateral data handlers only write to a channel.** They run on the client's read goroutine;
  issuing a command there deadlocks the connection.
- **Three connections per account.** One `idle` session (inbox only), one `sync`, one `interactive`,
  which serves every read and action. Gmail allows 15 for all of a user's apps together.
- **No reader bound to a connection crosses the provider boundary.** Parts and attachments are
  written to a temporary file while the session is held, and only then handed over.
- **Reading never changes the mailbox.** `BODY.PEEK` in a folder opened with `EXAMINE`; read tools
  never mutate (`get_message` does not mark a message read).
- **One tool, one class of annotation.** `trash_message` is separate from `move_message` because
  annotations are per tool.
- **No permanent deletion.** The only `\Deleted` + `UID EXPUNGE` in the daemon is the move fallback
  without `MOVE`, limited to the moved UIDs and requiring `UIDPLUS`.

### Sending

- **Sending needs confirmation.** `confirm: true` is required on every send, before anything is
  reserved or dialed. Never relax this in a test or behind a flag.
- **Idempotency is reserved in the database before the SMTP server is dialed.** A connection that
  drops or goes silent after the body started becomes `unknown`, which is never retried
  automatically (Exchange delivers both copies). A 4xx/5xx answer at the end of `DATA` is a refusal
  (RFC 5321): `failed` (`provider.ErrAfterData`), and never retried either.
- **Who may send is asked again** by `Outgoing.BeforeDial`, under the account's lock, before every
  connection. An SMTP AUTH refusal never changes the account's state: whether the grant died is
  IMAP's to say.
- **The Sent copy.** Never `APPEND` for Gmail and Microsoft, which file it themselves; only for
  generic IMAP with `save_sent_copy=1`.
- **RFC ids without brackets in the database.** The `<>` are added in one place, the go-mail
  builder. `msg_id` is the local foreign key and `message_id` is the header: never mix them.

### Data

- **Events.** The journal row and the state change are written in the same transaction, and
  published only after the commit. Replay always comes from the `events` table; there is no history
  in memory.
- **Secrets** are never stored in plain text, logged (the `obs` package redacts) or put in URLs.
  TLS is mandatory for IMAP and SMTP; `AllowInsecureAuth`, which allows a plain connection, is set
  only by tests.
- **Consent.** Nothing of a person's messages is stored before they consent to sync, and
  withdrawing deletes the index in the same transaction. Actions and sending each have their own
  consent, checked again right before the server is touched.
- **Backups: the host only encrypts and uploads.** It generates data keys with the encryption
  context exactly `{service: mailie, env: MAIL_ENV, purpose: db-backup, ref: <object key>}`
  (`backup.EncryptionContext`) and creates new objects under `db/` (`If-None-Match: *`, SSE-S3,
  never SSE-KMS); decrypting, reading, listing and deleting belong to another principal. Plaintext
  only in the run's 0700 directory. Never `query_only` on the snapshot connection (it refuses
  `VACUUM INTO`); `mode=ro` is the guarantee. AWS credentials come only from the SDK's default
  chain, never from `MAIL_*`. A restore trusts only the key it is given (`--kms-key-arn`), never the
  ARN in the header, which nothing authenticates.

## Tests

- `make check` is gofmt, vet, `lint-layout` and `go test -race ./...`, all offline: `imapmemserver`
  in process, a fake `go-smtp` server, `httptest` for OAuth, SQLite in `t.TempDir()`, MCP transports
  in memory.
- `make it` starts `it/compose.yml` (Dovecot, Mailpit) and runs `go test -tags integration`. It is
  the only place CONDSTORE, SPECIAL-USE and a real XOAUTH2 handshake run; CI requires it.
- `make web-check` is the console's typecheck, unit tests and build; CI runs it in a Node 24 job.
- The contract fixtures in `internal/api/testdata/contract/` are written by the real handlers
  (`go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`) and read by
  `web/test/contract.spec.ts`. Never edit them by hand or create them from the web side.
- Test names are sentences that state a guarantee (`TestAReadKeyCannotSend`), not `TestFuncName`.
