# Mailie

A Go server that exposes REST and MCP over several mail accounts (Gmail, Microsoft 365, iCloud,
generic IMAP). A local SQLite index; bodies and attachments on demand; IMAP IDLE for new mail;
sending over SMTP with XOAUTH2. `docs/architecture.md` is the long form of this file.

## Layout

- `cmd/mailserver` — one binary: `serve`, `account`, `apikey`, `user`, `migrate`,
  `rewrap-credentials`, `backup`, `mcp connect|install`. The CLI is a REST client of the daemon; only
  `apikey create --bootstrap`, `user invite|disable|delete|password --bootstrap` (`password` prints a
  reset invitation, never sets one), `migrate` and
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
  through a provider with `Service.SignInExternal`/`PinIdentityKey`, writes a first account key
  with the sign-in's ticket (`EnrolExternal`) and steps up through it
  (`MarkExternalStepUp`/`ExternalStepUp`, the product key compared with the pin, read only),
  suspends and resumes a person (`SuspendPerson`/`ResumePerson`), closes and reopens a team
  (`CloseTeam`/`ReopenTeam`) and stops the keys a person created before a moment
  (`RevokeKeysCreatedBefore`). Its `Deps` also run its background work (`Go`: started once every
  route is mounted, its context ended at the stop and awaited without a bound before the engine,
  the registry and the database close; a `Go` once the start has begun is dropped, logged at
  error), open its own secrets with the daemon's sealer (`OpenSecret`, only
  `secrets.ExtensionSecret(ref)`), register its metrics beside the core's (`Metrics`) and keep its
  own small state in the database it describes (`State(name)`, `extension_state`: restored with it,
  never ahead of it). `Options.Freshness` (never set by `serve`) is asked, outside every write,
  before a person's act that widens access or touches a team's shared data
  (`service.Freshness`, below); `Options.RemovedInSnapshot` (never set by `serve`) is what a
  followed workspace's snapshot says of a removed mailbox's people (`RemovedAsKept` unless set).
  Under a source that mirrors a directory the daemon names at start
  the local personal workspaces a document would not replace (`PersonalReplacementBlocks`).
  `Options.ExternalSignInOnly` (never set by `serve`) has the service
  refuse every password and invitation route, and `Options.TeamsWithheld` (never set by `serve`,
  an edition's choice, not a workspace source) every use of a team, by a person or a key: teams
  listed `withheld`, their mailboxes listed nowhere and synced by none (the store's rule), nothing
  of them deleted.
- `internal/config` — env only (`MAIL_*`) plus `.env`; `Load()` returns every error at once;
  `String()` redacts secrets, and names the credentials' sealer, never a KMS key's ARN.
- `internal/obs` — `log/slog` with redaction of addresses and credentials, Prometheus metrics.
- `internal/lockfile` — the data directory's lock: two daemons on one database corrupt the index.
- `internal/store` — SQLite (modernc, no cgo): a writer pool with `_txlock=immediate` and
  `MaxOpenConns(1)`, a reader pool with `query_only`, embedded `.sql` migrations plus
  `PRAGMA user_version`, FTS5. The mailbox journal (`mailboxevents.go`, migrations 0020 and 0021,
  a rebuild): `mailbox_events` (`removed` with its `purge_after`, `sync_paused` with the `cause`
  `bound_stopped` when a consent stopped with its person, `recovered`, `purged`, and `stranded`, a
  team mailbox its one reader lost with no protection asked, with its `cause` and `lost_user`),
  written in the transaction that does it with who read the mailbox then (`mailbox_event_readers`),
  never an address (a label, `MailboxLabel`, never one holding the address's local part, and a
  masked hint, forgotten when it is deleted for good), kept 90 days, read per workspace from a
  cursor by an extension (`mailbox_events_workspace`); and `notices`, a row per reader of a removal
  or a sync turned off, never for whoever acted nor of a stranding (a trigger), until dismissed,
  withdrawn by a recovery, or gone with its journal entry. For an extension that follows an
  external directory (migrations 0022 and 0023; empty on a self-hosted server): `directories`
  (generation, confirmation, reset count, cursor, published-through, one row per issuer, going back
  only with a reset), `directory_people` (what a directory said of a person, keyed by issuer and
  person, the subject unique among people still here, the revocation marks in microseconds only
  rising, a version falling only to 0 with a recorded reset, the row kept `gone_at` after the
  person's deletion until `ReleaseDirectoryPersonTx`), `workspaces.source_version`/`source_reset`,
  `workspace_members.held_at` (`HeldReaderSQL` says whom a hold keeps a mailbox for; it decides no
  access), `extension_state` (`extstate.go`, values of a mebibyte at most) and `workspace_changes`
  (`changes.go`: a mark per workspace from another source, written by triggers in the transaction
  of every change its snapshot shows, only rising). What an extension reads of them: the journal
  per workspace from a cursor, with its readers and a key's creator (`JournalOf`, `JournalHead`),
  the workspaces from another source (`FollowedWorkspaces`), and why a mailbox does not sync
  (`PauseSQL`, one order of causes beside `syncEligible`, empty exactly when it may sync, held to
  `SyncEligible` by test), all in one read when it must be (`ReadTx`, a deferred BEGIN on the
  query-only pool).
- `internal/secrets` — the `Sealer` interface (seal and open with a context and a `Binding`: one of
  five purposes, `credential/oauth-token`, `credential/password`, `send/hash-root`,
  `auth/kdf-salt-key` and `extension/secret` (`ExtensionSecret(ref)`: an extension's secret outside
  the database, which `serve` never seals, never rewrapped: sealed again and re-installed), and a
  ref, a credential's account id, a meta row or an extension's name for its secret, both held to a
  key service's encryption-context rule by every sealer;
  `Knows`/`Current` read only the header) and a `Composite` that seals with its active sealer and
  opens what any of its sealers knows. An envelope that does not open is only `ErrDecrypt`,
  `ErrUnknownKey` or `ErrMalformed` (`DoesNotOpen`); any other error is a sealer that could not
  try. `ErrSealedElsewhere` (with `ErrDecrypt`) is a KMS envelope of the configured key's kind that
  does not unwrap: another KMS key or `MAIL_ENV`, to put back, never a key to add or a rewrap. The `Keyring` is the self-hosted default: a versioned AES-256-GCM envelope
  (`v1||keyid||nonce||ct||tag`) with AAD binding the ref and the purpose's label (a credential's
  field, which never changes), rotation by key id, id 0 reserved for an envelope no keyring key
  sealed (`THCSEAL`, under a key service's data key). `kmssealer` is the other: the kit's `thcseal`
  over a `kms.Wrapper`, a data key per envelope under the context `{service: mailie, env:
  MAIL_ENV, purpose, ref}`, with no AWS SDK; its `Current` reads only the provider byte (the KMS
  key and the env are not in the header). `app.NewSealer` builds the configured one for the daemon
  and every command, and is the only place the kit's `awskms` client is made (depguard);
  `secretstest` has a second sealer and a fake `kms.Wrapper` for tests, never the kit's `localkek`.
- `internal/keyscheme` — Mailie's profile of the kit's key scheme (phase 3), parameters and checks
  only: account wraps bound to the seal id (never the address), the server's salt, the wrap under
  the product key, the browser vault's AAD, the seal domain `ML`/`mlv1` and kinds, grants at
  `GrantRow(namespace, namespace, seal id, epoch)`. The profile is the kit's `profiles/mailie`
  (v0.7.0), which `keyscheme.go` gives the core's names as aliases and forwarders, and
  `web/src/crypto/mailie.ts` re-exports from `@thehappieco/kit/profiles/mailie`; only the server's
  helpers stay here (`server.go`: the address as stored, the salt it hands out, drawing seal ids),
  and in the console `normaliseAddress`, `newNamespace` and `openBrowserVaultKey`. Its vectors
  (`testdata/`, `-update` on `TestTheVectorsAreWhatTheProfileWrites`) are written by Go and opened
  by `web/src/crypto/mailie.ts` (`web/test/keyscheme.spec.ts`); never edit them by hand. The kit
  froze them as its `vectors/mailie/key-scheme-v1`, held equal byte for byte
  (`TestTheVectorsAreTheKitsFrozenOnes`). A byte that changes is a new label, header or kind and a
  new version of `docs/key-scheme.md` and of the kit's vectors, never an edit. See
  `docs/key-scheme.md` and `docs/key-scheme-threat-model.md`.
- `internal/auth` — API keys `prefix.secret`, Argon2id PHC, scopes `read < write < send < admin`,
  expiry, revocation (a stopped key says why, `stop_reason`, migration 0017). Every key belongs to a workspace and acts as no person: an operator key
  (`wsp_operator`, any scope, optionally restricted to operator mailboxes) or a workspace key
  (`read`/`write`/`send`, holding its mailboxes in `key_access`, `internal/workspace/keys.go`);
  `Principal.IsInstance` means the operator workspace's. Also the console's people: users (instance
  roles `owner`/`member`; active, disabled here, or suspended by an extension (`Suspend`/`Resume`,
  `suspended_at`, migration 0018: `status = 'disabled'` with the grants kept, unusable, until a
  resume; a disable here clears it and stands over it); a password the server never receives: the
  key scheme's ceremonies, `accountkeys.go`, store an auth verifier, the account public key written
  once, the password and recovery wraps, the target salt and parameters; an old server-side password hash of a person who
  never enrolled is checked by nothing, and a reset invitation clears it), sessions (a 43-character opaque token stored as SHA-256, 14 days at most,
  never extended, with a step-up time), the ceremonies' single-use tickets and reset invitations,
  and single-use invites, to the instance or into a team.
  External identities (issuer + subject, linked only to the new person a first sign-in with a
  verified address creates; an existing address is a conflict, never a link) and their key pins
  (insert only, deleted only with the person, or by the hourly sweep when no sign-in linked them);
  a person who signs in that way has an empty password hash, which no password check accepts, and
  a session only once they have an account key: a first sign-in asking for the product key gets a
  single-use ticket, and its enrolment writes the public key once and the platform wrap
  (`platformwraps.go`, migration 0015: insert only, deleted with the person or by a reset).
  Deleting a person marks what an external directory said of them gone (`directory_people.gone_at`),
  and their name stays in the mailbox journal until an extension releases them; a person nobody
  follows is forgotten there at once, as actor and as the person a stranding lost. A disable, a
  deletion or a reset that leaves a team mailbox with nobody who reads it, would read it once
  resumed or would read it but for a hold journals it stranded (`disabled`, `deleted`, `reset`, by
  whoever did it). For applying a directory's document in one write, Tx forms that open no write
  of their own: `SuspendTx`, `ResumeTx` (`Resume` is a write around it),
  `RevokeSessionsStartedBeforeTx` (at microseconds; a session started in the moment's second ends
  too), `Keys.RevokeCreatedBeforeTx`; and `ExternalSignIn.PersonCreatedTx`, run in the transaction
  that creates a new person, right after their identity is linked, which rolls it all back on an
  error. `authtest` creates cheap users and keys for tests.
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
  token sources. Removing a mailbox keeps it 30 days (`removal.go`; `removed_at`, `purge_after`):
  `Get` and `Visibility` leave it out, so nothing finds, dials or refreshes it (`Registry.Mailbox`
  builds one only through `Get`, and caches none built while a removal let go of it), and only
  `GetRemoved`/`ListRemoved` read it; what keys held on it and its consent attempts go at once. A
  recovery clears the removal and holds its sync (`sync_held_at`) until someone who may turns it
  on. The hourly sweep, at start too, deletes it for good past its 30 days, never early, a closed
  team's included (`PurgeExpired`); a link that never connected (no credentials) is deleted at
  once, and so are the removed mailboxes that go with a person closed, their journal entries with
  them (a never-connected link in a workspace from another source is journaled `purged` instead,
  its earlier entries kept, for an extension to report). Its address stays taken in its workspace
  meanwhile (`ErrRemovedDuplicate`). A Google mailbox deleted for good — the sweep, its owners or
  the operator early, the person it goes with deleted or forgotten — has its grant revoked at
  Google (`revoke.go`, `GoogleRevokeURL`, which the daemon and `user delete --bootstrap` set): the
  refresh token opened inside the deleting transaction, revoked after the commit, best effort,
  bounded, logged without it, never while another mailbox here holds the same grant; Microsoft has
  no such endpoint and a password nothing to revoke.
- `internal/workspace` — workspaces (personal, team, the one operator workspace), members and
  per-mailbox grants (`read`, `act`, `send`, `manage`, which owners and admins hold by their role
  and members only as a stored flag), the protections each write keeps (last owner, last reader)
  and the workspace `Source` (local, or `Platform`, which mirrors an external directory: nothing of
  workspaces or memberships changes here, a person created gets no workspace from it, and the
  directory's documents send every one through `mirror.go`). A workspace's kind, source and
  person never change (migration 0016's trigger); a team closes and reopens (`SetClosed`,
  `closed_at`, migration 0019), and nothing in it is used meanwhile. Only a local team goes with its only member
  (`GoesWithPersonSQL`, the one rule for deleting it, its mailboxes and the last-reader test); a
  team whose source is not local outlives its people, its last one here included, and has no last
  owner here. `mirror.go` writes what a directory's document says: a team by the directory's id
  (`ValidExternalID`, never `wsp_…`), its name and closure taken only from a higher version than
  the one it took last (`source_version`); the person's personal workspace, replacing a local one
  only while it holds no mailbox, live or removed, and no live key (`ReplacePersonalTx`); the
  memberships it lists (`MirrorMemberTx`) and the end of the ones it omits (`EndMembershipTx`),
  without the refusals of a local change, held instead (`held_at`, migration 0022: disabled, its
  flags and grants kept) while the directory's generation is unconfirmed and the end would strand a
  mailbox. `strand.go` defines `Stranding` and journals the mailboxes a change strands
  (`JournalStrandedTx`); `StrandCandidatesTx` says which, before the change: the ones the person
  reads, would read once resumed or would read but for a hold, and nobody else does or would.
  `snapshot.go` reads who holds what on a workspace's mailboxes for an extension (`SnapshotTx`, in
  one read transaction): every mailbox, live and removed until deleted for good, its pause, label
  and masked hint, never its address; each person with a flag or a sealed grant, whether they read,
  wait, are enrolled and hold the key at its epoch; the people a team mailbox cannot lose. It
  decides nothing. See `docs/workspaces.md`.
- `internal/sync` — one worker per account, three sessions of fixed role (`idle`, `sync`,
  `interactive`).
- `internal/service` — the use cases and all authorization, access to mailboxes included; a
  concrete struct, not an interface. `directory.go` is what an extension that follows an external
  directory calls, with no principal and from no transport: a person's document applied in one
  write (`ApplyDirectoryDocument`, and at a first sign-in in the transaction that creates them,
  `ExternalSignIn.Directory`; a new person under the directory's source without one is not
  created), the directory's generation, resets, confirmation (which ends the held memberships) and
  feed cursor (a compare and set), a closed person forgotten (`ForgetExternalPerson`, a forced
  deletion refused by nothing) and released (`ReleaseDirectoryPerson`, or the 90-day
  `SweepDirectory`), and the listings it maps people and subjects with. `journal.go` and
  `snapshot.go` are what it reports: the journal as its outbox (`MailboxJournal`), the followed
  workspaces and their change marks, its own state, and a snapshot of a followed workspace
  (`AccessSnapshot`, which reports a removed mailbox's people as they would be once it is
  recovered, `RemovedAsKept`, unless `Deps.RemovedInSnapshot`, from `app.Options`, says otherwise). `freshness.go`: the
  hook (`Deps.Freshness`) asked before eleven acts of a person signed in — a key sealed to someone,
  flags raised for a person or a key, a key created, a link into a team, a team mailbox's consent
  started again, its sync switched, an action on it or a send from it — after the read-only
  authorization and outside every write; the write, or for those that write nothing first an
  immediate re-check, asks again who may, so an answer that ended the actor's place stops the act.
  Never asked for reading, removing, recovering, deleting for good, taking access away, or a key's
  own acts.
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
- **Sending needs confirmation.** `confirm: true` is required on every send, a person's or a
  key's; the two-step draft → send path is the planned alternative for MCP. Never relax this in a
  test or behind a flag. A workspace key sends through the same path, only from a mailbox it holds
  `send` on, with the `send` scope, while `MAIL_KEYS_MAY_SEND` allows (default true only under the
  open key terms: a server naming other key terms must set it), under the address alone, at most
  `DailyKeySendLimit` a day; its records and `send.finished` name the key. Idempotency is reserved
  in the database **before** dialing SMTP (a key-less send's key is the message, the sender and the
  minute), and a connection that
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
  Replay always comes from the `events` table — there is no history in memory. The mailbox journal
  (`mailbox_events`) is written the same way, in the transaction that removes, recovers, deletes for
  good, turns sync off or strands, and is not a record of `events`, which a deletion for good takes
  with the mailbox. It is an extension's outbox, read per workspace from a cursor; which workspaces
  from another source changed is marked by triggers in the same transaction (`workspace_changes`).
  A notice never goes to whoever acted, and a stream says `event: notice` (no `id`) only after the
  commit that gave one.
- **Secrets.** Never in plain text in the database, in logs (`obs` redacts) or in URLs. What must be
  opened again goes through a `secrets.Sealer`, with its binding and the caller's context: the
  credentials, the send-hash root (`meta.send_hash_root`, random, made at the first start, never
  derived from a key) and the salt key (`meta.kdf_salt_key`, K_salt of the key scheme, the same way,
  made only where people have passwords here), which the daemon refuses to start without opening. The sealer is the keyring
  (`MAIL_CREDENTIAL_KEY_HEX`) or AWS KMS (`MAIL_CREDENTIAL_KMS_KEY_ARN`, a key's full ARN, never an
  alias, with `MAIL_ENV` set; the keyring's keys then only open, and `MAIL_CREDENTIAL_SEALER=keyring`
  beside it is the way back, the KMS key only opening). The KMS provider takes the EC2 instance
  role's credentials through IMDSv2 only, unlike the backup's SDK default chain, and checks the key
  with `DescribeKey` before anything opens. `rewrap-credentials` opens the root and the salt key and
  re-seals them with the credentials under the active sealer; `--new-send-hash-root` and
  `--new-salt-key` replace one under another KMS key or `MAIL_ENV` only with `--kms-key-lost`. Only an envelope that does not open (`secrets.DoesNotOpen`)
  is a lost key: nothing replaces, or tells the operator to replace, what a sealer could not try. TLS
  is mandatory for IMAP/SMTP; `AllowInsecureAuth` is set only by tests.
- **Console session = bearer, never a cookie.** The token goes only in `Authorization`; sessions and
  API keys are told apart by their shape (a key has a dot). Person routes (`/v1/auth/*`) refuse API
  keys.
- **The server never receives a password** (`docs/key-scheme.md`, normative; its vectors are never
  edited by hand). A sign-in proves an auth key the browser derived under the salt and parameters
  the challenge answers (the address's target for every address without an enrolled account, so it
  says nothing of who has one); every way of failing costs one derivation against a dummy and gets
  one answer. A wrap is answered only to the secret verified in the same request, never to a
  session alone. The server checks shapes and stores only its default parameters and the target
  salt; it never makes or opens a key. `users.public_key` is written once (only a reset, which
  deletes every grant sealed to the old key, replaces it), `seal_id` never changes, and enrolment
  (`zk_enrolled_at`) is one way, with no password hash beside it: the schema holds all three.
  Changing the password and replacing the recovery code take the current auth key in the same
  request, never a session alone (a recovery code a session could set is a password it could set).
  Writing a mailbox key and a grant for someone else (linking, `read` given with a grant,
  supplying the key, a first or a new key) takes a step-up within ten minutes on that session,
  checked again in the transaction that writes (a sign-in counts, the hosted one is id.'s
  `auth_time`, never the sign-in's moment); flags alone take none. The browser makes every mailbox
  key and seals every grant (`web/src/crypto/mailbox.ts`); the server checks their shapes, writes
  each key and grant once, and never receives a private key. Every enrolment
  is told the seal id and the target before it seals: `signup/open` (the seal id drawn once per
  invitation, which `signup` must name) and `reset/open`; a reset's new password is
  derived under what `reset/open` answers (the target the reset stores), never the challenge's
  answer; without `--force` a reset refuses the last reader of any team mailbox that has a key (it
  deletes the person's grants; on a keyless mailbox they keep reading by the flag), a local team
  they are alone in included (closing a person leaves that team out: it goes with them). The upgrade's
  password in clear (`/v1/auth/upgrade/*`, the challenge's `upgrade`) existed in the release that
  brought the scheme only and is gone: a person who never enrolled signs in again only through a
  reset invitation. New passwords have at least twelve code points. The console's half
  (`web/src/crypto/account.ts`, `web/src/state/account.ts`) derives in the kit's worker, sends no
  password in any request, and keeps the account key only in
  the browser vault (`web/src/state/accountVault.ts`), wiped at sign-out and whenever no session is
  valid.
- **An external identity never takes over a person.** Accounts are never linked by matching
  addresses: a first sign-in through an extension only creates a new person, and an address that
  already has one here is `conflict`, with nothing created or linked.
- **Access to a mailbox is decided in `internal/service`.** Every mailbox belongs to its workspace
  (`accounts.owner_user_id` is the person of a personal workspace's mailbox, NULL for a team's and
  the operator's; who linked one is attribution only, `linked_by`). A person sees one as an active
  member of its workspace holding a grant on it, or as its owner or admin, who manage every
  mailbox of the workspace by their role — its card, re-authorizing, who holds what — and each use
  needs its flag. On a mailbox that has a key (`docs/key-scheme.md`, sections 8 and 9), `read` is
  the flag and a grant at its current epoch, one SQL rule (`store.ReaderSQL`) for access, listing,
  the last reader and sync eligibility; a member waiting for the key keeps the card and reads
  nothing, and taking `read` deletes the person's grants in the same transaction. An instance key
  reaches only the operator workspace's, whose mailboxes have no key. Another workspace's
  mailbox, or one the caller neither holds anything on nor manages, is `not_found`, never
  `forbidden`; listing filters in SQL with the same rule as fetching one. No role reads, acts or
  sends: `read` passes only from an owner or an admin who reads the mailbox now, `act` and `send`
  from any owner or admin (`act` to a reader), stored `manage` to members only. A team mailbox
  someone reads always keeps a reader (keys and roles never count). An API key belongs to its
  workspace: only its owners and admins, signed in, create, list and revoke keys (a key never
  mints one); a key reaches exactly what it holds in `key_access`, read live — `read` given only by
  an owner or an admin who reads that mailbox then, `act` and `send` by any of them — and keeps it
  whoever gave it; it never counts as a reader nor passes `read`, and is revoked when its creator
  leaves the workspace or is disabled, suspended or deleted (not on a demotion), and in every
  workspace when they recover their account or complete a reset (`account_recovered`, in that
  transaction; a password change and signing out everywhere keep it). A suspended person
  reads nothing, waits for no key, is supplied none and counts in no protection for anyone else
  (closing them asks it as for anyone active, counting what they would read once resumed); their
  grants persist for a resume, which restores no session and no key. A closed team (migration 0019) is a
  gate on use, never the reading rule (`store.OpenWorkspaceSQL` beside `ReaderSQL` in visibility,
  access, what a key holds and sync): its mailboxes reach nobody, its owners and keys included
  (`not_found`), and nothing in it changes, by anyone, until it reopens (`conflict`; the schema
  refuses what would give it something); its grants and keys are kept. Only the operator closes a
  local team; a team from another source closes where it is managed, through an extension. A
  removed mailbox (kept 30 days, `docs/workspaces.md`, "Removed mailboxes") is `not_found` to
  everyone, the operator included, but on the removed mailboxes' own paths; it has no reader and
  protects none (`store.LiveSQL`, inside `ReaderSQL` and `FlagHolderSQL`), and nothing is given on
  it (the schema refuses). Only its owners remove it — a personal mailbox's person, a team's active
  owners (an admin, only a link of their own that never connected), the operator for an operator
  mailbox — and they alone list, recover and delete it for good; the operator also lists and
  deletes any workspace's for good, and recovers no person's or team's. A recovery brings back the
  readers who still qualify and nothing a key held; none in a closed team, nor of a team mailbox
  nobody who could read it is left to read. **The directory exceptions**, under a source that
  mirrors an external directory (`docs/workspaces.md`, "Following an external directory"): what
  the directory says applies in a closed team too (memberships change while it is closed); its
  end of a membership, or of a person (`ForgetExternalPerson`), takes no refusal for the last
  owner, the last reader or a person who is not active, and may leave a team mailbox with no
  reader, journaled `stranded`; a membership it ends while its generation is unconfirmed, and
  whose end would strand a mailbox, is held (disabled, reading nothing, its flags and grants kept)
  until a document lists it again or the generation is confirmed; and a document whose workspace
  part is refused still applies what it revoked. A disable here stands over a directory: its
  documents never resume a person disabled here, nor one whose disable they do not know of. The
  freshness hook gates and never authorizes: every act it is asked about keeps its own checks, run
  again in the write.
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
- **Consent.** Nothing of a mailbox's messages is stored before its consent to sync: a personal
  mailbox's person's; a team mailbox's workspace's, which an owner or an admin gives on the team's
  behalf at the current sync text and any of them withdraws (on the mailbox: `sync_enabled_at`,
  `sync_enabled_by`, `sync_consent_version`); it also needs a reader. Withdrawing deletes the index
  in the same transaction: a person's withdrawal only their personal mailboxes', a team's for every
  reader, telling each of them but whoever turned it off with a notice. A removed mailbox keeps its
  index for its 30 days, syncing nothing, and loses it when deleted for good, or at once when its
  person withdraws or a removed team mailbox's owners turn it off. A recovered one syncs nothing,
  whatever its consent says, until someone who may turns it on (`sync_held_at`): its person, a
  team's owners and admins at the current sync text, the operator. A team consent migration 0011
  copied from its linker (`sync_enabled_via = 'migration'`) stays bound to them until confirmed:
  their withdrawal, disable or deletion stops it and deletes its index, telling its other readers
  (closing them is refused without force while someone else reads it); their suspension
  pauses it, as it pauses their personal mailboxes, index kept. A closed team's mailboxes sync
  nothing, their index and consent kept, until it reopens. A team mailbox
  nobody can read is never switched on. Closing a person without force is refused for the last
  reader of a team that outlives them (one another member remains in, whatever their status, or
  one whose source is not local), the same test as whether the team goes with them. Actions and sending have their own consents, the actor's own,
  checked again right before the mail server is touched; a workspace key's are the key terms its
  creator agreed to (recorded on the key), with the key, its flag and its scope checked again
  instead, so no text says a person's own switch stops a key created since 0012. The revisions come
  from
  `MAIL_CONSENT_VERSION_*`, whose defaults are the open console's texts
  (`web/src/open/versions.ts`).
- **Errors.** Sentinels `errors.New("pkg: ...")`, always `errors.Is`/`errors.As`. The API answers
  `{code, message}` with seven fixed codes and never says which credential failed. What an
  extension calls keeps the cause of each refusal for `errors.Is` (`service.ErrNotFollowing`,
  `ErrDirectoryGeneration`, `ErrKindChanged`…); a new person under a directory's source without
  their document is `internal`, "upstream: …", with a `Retry` of 10 s; so is an act the freshness
  hook could not confirm (a `Retry` of 30 s, no reason on the wire), while a `*service.Error` the
  hook returns passes through as it is and a context's end is passed on.
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
