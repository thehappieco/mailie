# Architecture

Mailie is one Go binary (`cmd/mailserver`) over one SQLite database, plus a web console built
separately (`web/`). This page is the map for contributors: where things live, and the rules that
are easy to break and expensive when broken. Most of them exist because the alternative loses mail,
sends it twice or leaks a credential.

## Layout

| Path | What it holds |
|---|---|
| `cmd/mailserver` | The binary: `serve`, `account`, `apikey`, `user`, `migrate`, `rewrap-credentials`, `backup`, `mcp connect\|install`. The command line is a REST client of the daemon. Only `apikey create --bootstrap`, `user invite\|disable\|delete\|password --bootstrap`, `migrate` and `rewrap-credentials` open the database directly, and they refuse to run while the daemon holds its lock. `backup` is the one exception to the lock: it reads the database beside a running daemon through a `mode=ro` connection that writes nothing. `backup` and `backup restore` load their own configuration (`config.LoadBackup`/`LoadRestore`) before `config.Load`, so a restore runs where none of the daemon's variables exist. `mcp connect` and `mcp install` run beside an MCP client on any machine, before `.env` and `config.Load`: they need no `MAIL_*` variable and open no database ([`mcp.md`](mcp.md#connect-a-client)). |
| `internal/app` | The daemon's assembly, which `serve` calls (`app.Run`): the lock, the sealer (`app.NewSealer`, which every command that opens a sealed value builds too: the keyring, or under `MAIL_CREDENTIAL_KMS_KEY_ARN` a `secrets.Composite` of the KMS sealer and the keyring's keys, which only open, or, with `MAIL_CREDENTIAL_SEALER=keyring` beside it, the way back, of the keyring and the KMS sealer, which only opens; the one place the kit's `awskms` client is made, with instance-role credentials through IMDSv2 only and `DescribeKey` before the database is opened), the store, the send-hash root and the salt key (only where people have passwords here), the registry and the sync engine, the service, REST, MCP over HTTP and stdio, the console, metrics, the listeners, the startup hints, the retention sweeps (unused invites, keys pinned for an identity no sign-in linked, expired tickets and reset invitations, send records) and a bounded shutdown. Another binary that must run the same server calls it too. Its one extension point, `Options.Extensions`, mounts routes before the console's catch-all and refuses at start any that is malformed, claims the root, names a host (which the mux would try before every core route), or duplicates, conflicts with or would take a request from a core route, judged with every core route switched on. An extension may sign people in through an identity provider (`Service.SignInExternal`, `Service.PinIdentityKey`); `Options.ExternalSignInOnly`, which only such a binary sets, has the service refuse every password and invitation route ([`console.md`](console.md#signing-in-through-an-extension)). |
| `internal/config` | Environment only (`MAIL_*`, optionally seeded from `.env`). `Load()` returns every error at once; `String()` redacts secrets and names what seals the credentials (`credential_sealer=keyring`, `aws-kms`, or `keyring,opens:aws-kms` on the way back), never a KMS key's ARN. |
| `internal/obs` | `log/slog` with redaction of addresses and credentials; Prometheus metrics. |
| `internal/lockfile` | The data directory's lock: two daemons on one database would corrupt the index. |
| `internal/store` | SQLite through `modernc.org/sqlite` (no cgo): a writer pool with `_txlock=immediate` and `MaxOpenConns(1)`, a reader pool with `query_only`, embedded `.sql` migrations tracked by `PRAGMA user_version` (a table is rebuilt only through the runner's rebuild procedure, below), FTS5. |
| `internal/secrets` | The `Sealer` interface: seal and open with a context and a `Binding` (the purpose, one of `credential/oauth-token`, `credential/password`, `send/hash-root` and `auth/kdf-salt-key`, the only ones a key service's policy has to allow; the ref, a credential's account id or a meta row; both held by every sealer to the encryption context's rule), say without opening an envelope whether it is known and whether it is current; a `Composite` seals with its active sealer and opens what any of its sealers knows. An envelope that does not open is reported only as `ErrDecrypt`, `ErrUnknownKey` or `ErrMalformed` (`DoesNotOpen`); any other error says the sealer could not try. The `Keyring`, the self-hosted default, is a versioned AES-256-GCM envelope (`v1‖keyid‖nonce‖ct‖tag`) whose additional data binds the ref and the purpose's label (for a credential, its field, as every stored credential was sealed); rotation by key id, and id 0 is reserved for an envelope no keyring key sealed (a `THCSEAL` one, under a key service's data key). `kmssealer` is the other sealer: a `THCSEAL` v1 envelope (the kit's `thcseal`) per secret, with a data key of its own that a `kms.Wrapper` makes and unwraps under the encryption context `{service: mailie, env: MAIL_ENV, purpose, ref}` (`kmssealer.EncryptionContext`); it knows every `THCSEAL` envelope, holds one current when its provider byte is the wrapper's (the KMS key that wrapped it is inside KMS's undocumented ciphertext, and the env and binding only in the additional data, so neither is decidable from the header), and reports another provider's envelope as `ErrUnknownKey`, one that does not unwrap or authenticate as `ErrDecrypt` and `ErrSealedElsewhere` (most likely another KMS key or another `MAIL_ENV`: settings to put back, which no key given beside it and no rewrap can stand in for), and any other KMS error as it is. It links no AWS SDK. `secretstest` has a second sealer and a fake `kms.Wrapper` for tests. |
| `internal/keyscheme` | Mailie's profile of the kit's key scheme, phase 3: the account profile (labels, the platform's password preparation and KDF bounds, base64url, the platform's recovery code), the 61-byte password and recovery wraps of an account key bound to the person's seal id, kind and public key, the server's salt for an address, the wrap under the product key (the kit's Mailie profile, bound to the seal id), the browser vault's additional data, the seal domain (`ML`, `mlv1`) and kinds, and grants of a mailbox's key at `GrantRow(namespace, namespace, seal id, epoch)` that open only to the mailbox's public key. Parameters and checks only, no primitive of its own; the server's checks are `CheckAccountWrapShape`, `CheckGrantShape`, `CheckPublicKey` and the spellings of seal ids and namespaces. The profile is the kit's `profiles/mailie` (v0.7.0), named here by aliases and forwarders; the server's salt, the address as it is stored and drawing seal ids stay in `server.go`. Its golden vectors in `testdata/`, written by Go, opened by `web/src/crypto/mailie.ts` (which re-exports the kit's TypeScript profile) and frozen by the kit as its `vectors/mailie/key-scheme-v1`. See [`key-scheme.md`](key-scheme.md) and [`key-scheme-threat-model.md`](key-scheme-threat-model.md). |
| `internal/auth` | API keys `prefix.secret` hashed with Argon2id (PHC strings), scopes `read < write < send < admin`, restriction to accounts, expiry, revocation. Also the console's people: users (instance roles `owner`/`member`) and a password the server never receives (`accountkeys.go`, the key scheme's ceremonies of [`key-scheme.md`](key-scheme.md) section 12: the challenge, sign-in with an auth key, sign-up, the two-step password change, recovery, the step-up, the upgrade's one check of an old password hash, the reset invitation; the auth verifier, the account public key written once, the wraps and the target salt), sessions (a 43-character opaque token stored as SHA-256, 14 days at most, never extended, with a step-up time), the ceremonies' single-use tickets, and single-use invitations, to the instance or to a team; and the identities an identity provider names a person by (issuer and subject, linked only to the new person a first sign-in with a verified address creates: an address somebody here has already is a conflict, never a link), with the public keys pinned for each, never replaced and deleted only with their person. A person who signs in only that way has no password, and no auth key signs them in. `authtest` mints cheap users and keys for tests. |
| `internal/workspace` | Workspaces (a person's personal one, teams, the one operator workspace), their members and the per-mailbox grants (`read`, `act`, `send`, and `manage`, which owners and admins hold by their role and members only as a stored flag), with the protections that hold inside each write: a team keeps an active owner, and a team mailbox someone reads keeps a reader. The `Source` (local, or the platform's) says where workspaces come from. Who may change what is the service's. See [`workspaces.md`](workspaces.md). |
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
  `sync`, `provider`, `account` or `workspace`. If a rule appears on both sides, it is in the wrong place.
- **Access is decided in `internal/service`.** Every mailbox belongs to its workspace. A person
  sees one as an active member of that workspace who holds a grant on it (`read`, `act`, `send`,
  `manage`) or, as its owner or admin, manages it by their role, and each use needs its flag;
  an instance key reaches only the operator workspace's mailboxes. Another workspace's mailbox, or
  one the caller neither holds anything on nor manages, is `not_found`, never `forbidden`, and
  listing filters in SQL with the same rule as fetching one. No role — a team's owner or admin, or
  the instance's `owner` — reads, acts on or sends from a mailbox by being one: an owner or an
  admin sees its card and decides who holds what, `read` passes only from one of them who reads
  it, and `act` and `send` from any of them. See [`workspaces.md`](workspaces.md).
- **A console session is a bearer token, never a cookie.** It travels only in `Authorization`;
  sessions and API keys are told apart by their shape (a key has a dot). Person routes
  (`/v1/auth/*`) refuse API keys. A session lasts what it was started for, 14 days or less for one
  an extension starts (`SignInExternal`), and nothing extends it; the schema refuses an update that
  moves an expiry later and an insert that would replace a session. A personal key created in such
  a session lasts what the person chose for it.
- **An external identity never takes over a person.** Accounts are never linked by matching
  addresses: an identity seen for the first time only creates a new person, and an address somebody
  here has already, however they sign in, is `conflict`, with nothing created or linked.
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

- **Rebuilding a table.** SQLite drops a constraint only by rebuilding the table, and an ordinary
  migration runs with foreign keys on, where `DROP TABLE` cascades into every child: dropping
  `accounts` would delete every credential, folder and message. A migration that rebuilds a table
  says so on its first line (`-- migration: rebuild`) and runs through the runner's rebuild
  procedure (`store/migrate.go`): foreign keys off outside the transaction and read back, the rows
  of every table counted, the new table created and filled with every column named, the old one
  dropped and the new one renamed into its place (never the old one renamed aside first),
  `foreign_key_check` and the counts again before the commit, foreign keys back on whatever
  happened. A test fails any other migration that drops a table something refers to.
- **A schema newer than the binary** is refused (`store.ErrSchemaTooNew`): a binary never runs on a
  database a newer one migrated. Going back is the backup taken before the upgrade, with the binary
  of its time.
- **Events.** The journal row and the state change are written in the same transaction, and
  published only after the commit. Replay always comes from the `events` table; there is no history
  in memory.
- **Secrets** are never stored in plain text, logged (the `obs` package redacts) or put in URLs.
  What the server must open again is sealed by a `secrets.Sealer`, for its binding, under the
  context of the call that needs it: the credentials, and the send-hash root (the key of a send
  record's hashes), which is random, made at the first start, kept in `meta` sealed and never
  derived from a key. The sealer is the keyring of `MAIL_CREDENTIAL_KEY_HEX`, or, with
  `MAIL_CREDENTIAL_KMS_KEY_ARN`, AWS KMS: a `THCSEAL` envelope per secret under a data key KMS
  wraps for the context `{service: mailie, env, purpose, ref}`, reached with the EC2 instance
  role's credentials through IMDSv2 only (unlike the backup, which takes the SDK's default chain),
  the keyring's keys then only opening what they sealed before. A root the configured keys do not
  open stops the daemon at start, naming what sealed it (`meta.send_hash_root_sealed_with`, kept
  beside the root for messages only); `rewrap-credentials` opens the root and re-seals the
  credentials and the root with the active sealer, which is how a server moves from the keyring to
  KMS, and back with `MAIL_CREDENTIAL_SEALER=keyring`. Only an envelope that does not open
  (`secrets.DoesNotOpen`) counts as a lost key: a sealer that could not try (a key service not
  reached, throttling, or that refused the call) stops the start too, but nothing then replaces the
  root or tells the operator to, and a root under another KMS key or `MAIL_ENV`
  (`secrets.ErrSealedElsewhere`) is replaced only with `--kms-key-lost`. TLS is mandatory for IMAP and SMTP;
  `AllowInsecureAuth`, which allows a plain connection, is set only by tests.
- **Consent.** Nothing of a mailbox's messages is stored before its consent to sync: a personal
  mailbox's person's, a team mailbox's workspace's (given by an owner or an admin on the team's
  behalf, recorded on the mailbox with who gave it, when and to which text). Withdrawing deletes
  the index in the same transaction: a person's withdrawal deletes their personal mailboxes'
  index, and a team's only where the team's consent is still the one migration 0011 copied from
  theirs (bound to them until an owner or an admin confirms it); switching a team mailbox off
  deletes it for everyone who reads it. A team mailbox nobody can read syncs nothing and is never
  switched on again.
  Actions and sending each have their own consent, the actor's own, checked again right before
  the server is touched.
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
