# Mailie

A Go server that exposes REST over several mail accounts (Gmail, Microsoft 365, iCloud, generic
IMAP). A local SQLite index; bodies and attachments on demand; IMAP IDLE for new mail; sending over
SMTP with XOAUTH2. A self-hosted server has no people and serves no MCP of its own (platform
decision 0065): an operator, instance keys made with the CLI, and REST. `docs/architecture.md` is
the long form of this file.

## Layout

- `cmd/mailserver` — one binary: `serve` (no flag), `account`, `apikey`, `user disable|delete`,
  `workspace list|rename|close|reopen`, `member list|remove`, `access list|revoke`, `migrate`,
  `rewrap-credentials`, `backup`; `apikey hold|release SELECTOR`, beside
  `apikey create|list|revoke`, holds and releases a key an extension's issuer made (the operator's
  stop, `PUT|DELETE /v1/issued-keys/{selector}/hold`, outside `MAIL_ADMIN_API`). The CLI is a REST client of the daemon; only
  `apikey create --bootstrap`, `user disable|delete --bootstrap`, `migrate` and
  `rewrap-credentials` open the database directly, and they refuse to run while the daemon is up.
  The one exception to the lock is `backup`: it reads the database with the daemon up, through a
  `mode=ro` connection that writes nothing, and takes no lock. `backup` and `backup restore` load
  their own configuration (`config.LoadBackup`/`LoadRestore`) before `config.Load`; a restore runs
  wherever the backups may be decrypted, with none of the daemon's variables (`make backup-tool`
  builds a Linux binary for it). The CLI invites nobody and sets no password (platform decision
  0065): `user invite` and `user password` are named only to refuse, a plain error before anything
  is opened or asked; `workspace create`, `member role` and `access grant` are gone (usage); the
  people commands left list, rename, close, reopen and take away, for a database the open console
  left (the README's "Moving from the open console"). `mcp`, whatever follows, is `errNoMCP`
  (exit 1), answered before `.env` and `config.Load`; `mcp connect|install` and
  `internal/mcpbridge` are gone. A mailbox's password is never an argument: `account add
  --password` takes only `-` (typed at a prompt that does
  not show it, or the first line of standard input, its line ending alone taken off, at most 4096
  bytes; any other value refused before anything is sent, `errPasswordArgument`; `hidden.go`, the
  portable hidden entry), and `account
  add` and `account authorize` say where the mailbox's sync stands, as the daemon answered it
  (`mailboxSyncNote`: held, paused, on, else off until `account sync ID on`). `account authorize`
  of a mailbox that is `active` never says authorised: a working mailbox's new grant changes no
  state the daemon exposes, so loopback opens the consent page and returns (`showConsentPage`) and
  `--device` is refused before anything starts; `--paste` and a failed or pending mailbox wait as
  before.
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
  `secrets.ExtensionSecret(ref)`), register its metrics beside the core's (`Metrics`), keep its
  own small state in the database it describes (`State(name)`, `extension_state`: restored with it,
  never ahead of it) and hand back what a route of its takes below a prefix and does not answer
  (`Console`: what the core mounts at `/`, the console with `Options.Pages`, or without one the
  mux's own 404, never nil). `Options.Freshness` (never set by `serve`) is asked, outside every write,
  before a person's act that widens access or touches a team's shared data
  (`service.Freshness`, below). Under a source that mirrors a directory the daemon names at start
  the local personal workspaces a document would not replace (`PersonalReplacementBlocks`).
  `Options.ExternalSignInOnly` (never set by `serve`) has the service
  refuse every password and invitation route (`errExternalSignInOnly`) and take its extension's
  sessions whatever `MAIL_PASSWORD_SIGN_IN` says; without it, `cfg.PasswordSignIn` decides whether
  anybody signs in (`Deps.PasswordSignIn`), and the salt key is made and opened only where it is on
  and `ExternalSignInOnly` is not (`openKDFSaltKey`; anywhere else none is made, one a database holds
  left unopened). `Options.TeamsWithheld` (never set by `serve`,
  an edition's choice, not a workspace source) refuses every use of a team, by a person or a key: teams
  listed `withheld`, their mailboxes listed nowhere and synced by none (the store's rule), nothing
  of them deleted. `Options.KeysInPersonalWorkspaceOnly` (never set by `serve`, an edition's
  choice too) keeps API keys to personal workspaces: creating a key in a team and raising what a
  key holds on a team's mailbox are `not_authorized` (`errTeamKeys`), and no key reaches a team's
  mailbox (one a database still holds included: listed nowhere, refused by name), while listing a
  team's keys, revoking them and taking away stay. `Options.SyncPauseOffered` (set by `serve`, for
  the operator's `account sync ID pause|resume`; another binary sets it only once its sync text
  says what a pause does) offers
  a person's pause of one mailbox's sync: left unset, the core refuses every such pause
  (`not_authorized`), never a recovery's pause nor a resume, so an edition offers it by opting in,
  never by forgetting to withhold it. `Options.NameFollowsIdentity` (never set by `serve`) names a
  person linked to an external identity as the provider names them at every sign-in
  (`SignInExternal`, in its
  transaction; an empty name leaves them none), never at a step-up nor from anywhere else, and
  refuses their `PUT /v1/auth/profile` (`conflict`, `auth.ErrNameFromIdentity`); a person with a
  password names themselves as ever. `Options.Pages` (never set by `serve`; `webui.Pages`) adds
  headers of the binary's own to the console's page documents it chooses (`Document`, given the
  request: never a header that protects the page or describes the answer, never a referrer
  policy that sends another origin the path; `Whole`: `no-store`, conditional and range headers
  ignored) and serves small files beside them (`Files`: their own type, `no-store`, whole),
  refused at start, `webui.ErrPages`, for a path the console would not serve.
  `Options.KeyIssuer` (never set by `serve`; `service.KeyIssuer`) is an extension whose API keys
  are made elsewhere and mirrored here, refused at start unless `ExternalSignInOnly`
  (`app.ErrKeyIssuer`). `Options.KeysIssuedElsewhere` (never set by `serve`) withdraws this
  server's own keys: creating one and raising what one holds are `not_authorized`
  (`errKeysIssuedElsewhere`) before anything else is asked, the freshness hook included, an own
  key outside the operator workspace is refused as invalid, and every live one is revoked at
  start, before anything listens (`Service.RevokeOwnWorkspaceKeys`, `manual`, logged with
  `revoked`, `unexpired` and `at`; the operator's kept). `Options.IssuedKeysRefused` (never set by
  `serve`) refuses every issued key, at authentication and every re-check, while their records
  still apply, and reports each live one stopped (`withdrawn`). The hourly housekeeping, at start
  too, records the end of the issued keys that expired and deletes what is no longer kept of them
  (`EndExpiredIssuedKeys`, `SweepIssuedKeys`). It serves no MCP (no `Options.MCPStdio`, no
  `mcp.New`): `coreRoutes` mounts `/mcp` and `/mcp/` as `webui.NotFound()`, the API's JSON 404 for
  every method, with a console or without, and the start says once at warn which retired
  variables still ask for MCP (`cfg.RetiredMCP`, names only, `variables`). With no key it warns
  `no api keys exist yet; nothing can call this daemon`, or says at info that only console sessions
  can where a page is served and people sign in; the invitation hints (`firstInviteHint`) only where
  people sign in with a password and `MAIL_WEB_DIR` or `MAIL_PUBLIC_URL` is set. At every start, a
  page or none, the daemon warns of the mailboxes outside the operator workspace, which no instance
  key reaches (`warnWorkspaceMailboxes`, `workspacemailboxes.go`: one line at warn with
  `personal_live`, `personal_removed`, `team_live` and `team_removed`, and a hint naming the
  README's "Moving from the open console"; counts only, never an address, a label or an id;
  nothing when there are none, and never under `ExternalSignInOnly`; a count that fails is logged
  and never stops the start).
- `internal/config` — env only (`MAIL_*`) plus `.env`; `Load()` returns every error at once;
  `String()` redacts secrets, and names the credentials' sealer, never a KMS key's ARN.
  `PasswordSignIn` (`MAIL_PASSWORD_SIGN_IN`, default false, beside `KeysMaySend`; `password_sign_in`
  in `String()`): whether people sign in with a password here; off, the server has no people.
  `RetiredMCP` names, in order, `MAIL_MCP_HTTP`, `MAIL_MCP_KEY` and `MAIL_MCP_ELICIT_SEND` where the
  environment sets one to ask for MCP (the key when non-empty, the two booleans unless
  `strconv.ParseBool` reads false; empty is unset): read by nothing else, refused by nothing.
- `internal/obs` — `log/slog` with redaction of addresses and credentials (an own key's secret
  after its prefix, and the generic shape of an issued key's after its selector), Prometheus
  metrics.
- `internal/lockfile` — the data directory's lock: two daemons on one database corrupt the index.
- `internal/store` — SQLite (modernc, no cgo): a writer pool with `_txlock=immediate` and
  `MaxOpenConns(1)`, a reader pool with `query_only`, embedded `.sql` migrations plus
  `PRAGMA user_version`, FTS5. The mailbox journal (`mailboxevents.go`, migrations 0020, 0021 and
  0025, the last two rebuilds): `mailbox_events` (`removed` with its `purge_after`, `sync_paused` with the `cause`
  `bound_stopped` when a consent stopped with its person, `recovered`, `purged`, and `stranded`, a
  team mailbox its one reader lost with no protection asked, with its `cause` and `lost_user`, and
  `paused` and `resumed`, a team mailbox's sync paused by a person with its index kept, and any
  team pause ended by a resume (never a cause; a turn-off stays `sync_paused`; a recovery's pause
  is its `recovered` entry; every other end of a team pause is journaled by the act that ends it,
  `sync_paused`, `removed` or `purged`),
  written in the transaction that does it with who read the mailbox then (`mailbox_event_readers`),
  never an address (a label, `MailboxLabel`, never one holding the address's local part, which
  0021's copy left out of the older entries too, and a masked hint, forgotten when it is deleted
  for good), kept 90 days, read per workspace from a
  cursor by an extension (`mailbox_events_workspace`); and `notices`, a row per reader of a removal,
  a sync turned off or a person's pause, never for whoever acted nor of a stranding or a resume (a
  trigger), until dismissed, withdrawn by a recovery or by the pause's end, or gone with its
  journal entry, a pause's at 90 days though the pause has no time limit (its card says it). For an extension that follows an
  external directory (migrations 0022 and 0023; empty on a self-hosted server): `directories`
  (generation, confirmation, reset count, cursor, published-through, one row per issuer, going back
  only with a reset), `directory_people` (what a directory said of a person, keyed by issuer and
  person, the subject unique among people still here, the revocation marks in microseconds only
  rising, a version falling only to 0 with a recorded reset, the row kept `gone_at` after the
  person's deletion until `ReleaseDirectoryPersonTx`), `workspaces.source_version`/`source_reset`,
  `workspace_members.held_at` (`HeldReaderSQL` says whom a hold keeps a mailbox for; it decides no
  access), `extension_state` (`extstate.go`, values of a mebibyte at most, listed by prefix as a
  range of the primary key, `ExtensionStateKeys`, never a scan of the extension's keys) and
  `workspace_changes`
  (`changes.go`: a mark per workspace from another source, written by triggers in the transaction
  of every change its snapshot or its settings show, only rising; since migration 0027 a mailbox's
  reason, attribution, agreement and its text, who paused it, sent copy, host, first connection
  and server too, never its sync bookkeeping, folders or messages, which a report measures).
  Migration 0027 adds `person_changes` (`changes.go`: a mark per person an identity signs in, in
  the transaction of every change to their own settings as reported — consents, account key,
  sign-out everywhere, a notice given or gone or changed by its mailbox, label, hint or actor's
  membership — one per person however many notices one write touched, the triggers selecting
  each holder once; gone with the person's identity; empty on a self-hosted server),
  `users.signed_out_everywhere_at` (written by a sign-out everywhere alone, a person's signed in or
  one asked through an extension, never moved back), `accounts.connected_at`
  (the first credential's moment, by trigger; known for password mailboxes and every mailbox
  connected since, 0 for an OAuth one connected before) and `accounts.server_kind` (`gmail`,
  `other`, or empty until a login records it). What an extension reads of them: the journal
  per workspace from a cursor, with its readers and a key's creator (`JournalOf`, `JournalHead`),
  the workspaces from another source (`FollowedWorkspaces`), a person's settings and notices from
  the journal's label and hint, never an address (`PersonSettingsTx`, `personsettings.go`), which
  people changed (`PersonChanges`), and why a mailbox does not sync
  (`PauseSQL`, one order of causes beside the engine's rule, a pause, `paused`, last, empty
  exactly when it may sync, held to `SyncEligible` by test), all in one read when it must be (`ReadTx`, a deferred BEGIN on the
  query-only pool). Migration 0028 (`docs/workspaces.md`, "Keys an issuer makes"; empty on a
  self-hosted server) adds `api_keys.issuer` (`''` for this server's own keys; an issued key always
  expires, is never the operator's, has no name, origin or last use, the `write` scope and a
  `usr_…` creator, and its issuer, hash, creator, creation and workspace never change, nor does
  its revocation undo or its expiry move later) and `issued_keys` (id at the issuer, version,
  listed, creation in microseconds, terms judged once, end and how), `issued_key_stops`
  (`operator`, `creator`, each lifted alone), `issued_key_grants` (per mailbox and use: giver's
  subject and time, held, waiting, refused with a cause, or a removed tombstone, which a mailbox's
  removal makes of the held and waiting ones), `issued_key_uses`, `issued_key_tell` (owed to the
  issuer), `issued_key_ends` (the selector, how and when, written by `issued_keys`' delete trigger
  on every path, the cascade from a workspace included, never changed, never stored again) and
  `directory_keys` (per issuer, its feed's cursor under its directory's reset, what it trusted
  before the last reset, its pass and when a page last applied); the workspace marks follow
  `key_access`, an issued key's revocation, expiry, end and stops. `ResetDirectoryTx` starts an
  issuer's keys again in the same write (`resetIssuedKeysTx`: the feed from 0 with its pass open,
  what it trusted never lowered, every key and grant at version 0, every refusal owed dropped,
  nothing else), and `SetDirectoryKeysCursorTx` is the feed's compare and set. `IsConstraint`
  tells any schema refusal from another failure; `IssuedKey*Retention` are the periods kept.
  Migration 0029 (`docs/workspaces.md`, "Acts an extension runs for a person") adds
  `extension_requests` (`requests.go`: a request an extension carries for a person, by extension
  and id, its person with no foreign key, its act, what it did and its `keep_until`; written by
  the write that decides it, in its transaction: `BeginRequestTx` first — a request that ran is
  `*RequestDone` with its first outcome, a person who may no longer act `ErrActorInactive`
  (`RequirePersonActsTx`), a mailbox outside its workspace `ErrNotInWorkspace` — and
  `RecordRequestTx` last; `WriteRequest` wraps both, `RecordNoOp` records a request whose effect
  held already, `SweepRequests` deletes the records past their end) and `mailbox_access.raised_at`
  (moved only by a change that adds a flag, never back; a request's widening is stamped with its
  first moment, `Request.Stamp`, never before a later moment it finds, and a narrowing refused,
  `ErrSetSince`, for what was given at or after its first second). Each decisive write takes the
  request in an `…For` form (`PauseSyncFor`,
  `SetMailboxSyncFor`, the consents' `Grant…For`/`Withdraw…For`; `workspace.SetGrantFor`,
  `RevokeFor`; `account.Registry.RemoveFor`, `RecoverFor`, `PurgeFor`, which refuses
  `ErrSetSince` for a mailbox removed at or after the request's first second, recovered and
  removed again since), the old name a nil request; and the writes only a request had before:
  `SyncOffRemovedFor` (a removed mailbox's own
  sync off, its agreement compared as a turn-off's, which leaves one live again as it is),
  `ForgetRemovedIndexFor` (a removed personal mailbox's kept index, at its person's word: its
  consent, hold and removal kept, nothing journaled), both refusing `ErrSetSince` for a mailbox
  removed at or after the request's first second (recovered and removed again since), and
  `DismissNoticesFor` (a person's notices by journal id, at most `MaxNoticesDismissed`, an id they
  hold none of done); `PurgedIn` reads a deletion for good from a workspace's journal.
  What a mailbox holds (`usage.go`, `Usage`): usage rule 1 (`RuleVersion`), logical and
  deterministic, the same rows giving the same figure on every server and after every `VACUUM`:
  per mailbox, every value of its `messages` rows still in their folder (a copy per folder), their
  `parts`, its `folders` and its open `drafts` (`octet_length` for text and blobs, 8 for a
  non-null number, 0 for `NULL`), in one statement; never the full-text index, any index's
  overhead, the `bodies` cache, attachments, sends, the journals, notices, keys or grants; beside
  it `ProviderBytes`, the `RFC822.SIZE` sum. The columns it counts are written out
  (`usageTables`) and held to the schema by `TestTheUsageRuleCoversEveryColumnOfTheTablesItCounts`:
  a migration that touches `messages`, `parts`, `folders` or `drafts` lists its columns there and
  decides whether `RuleVersion` moves. `CountWorkspaceMailboxes` (`workspacemailboxes.go`) counts
  the mailboxes outside the operator workspace, personal and team, live and removed, whatever their
  state, for the start-up warning.
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
  `Principal.IsInstance` means the operator workspace's. A key an issuer made (`issued.go`,
  migration 0028) is a workspace key's principal with `Principal.Issuer` set and its selector as
  `KeyPrefix`, checked by `AuthenticateIssued` (one indexed read, the issuer's constant-time
  comparison, never Argon2id, `ErrInvalidKey` for every refusal, `IssuedCheck` naming the key only
  once its secret matched), never touching `last_used_at`; `Recheck` refuses one stopped here, and
  every own-key listing, count, revocation and `key_access` change reads `issuer = ''` only.
  `IsSessionToken` is a session token's shape, which no issuer is asked to claim. Own-key
  revocations (`endWaysInTx`, `RevokeCreatedBefore`) read `issuer = ''`: a disable here gives the
  person's issued keys the `creator` stop instead (`stopIssuedKeysOfTx`, never revoked here),
  `Suspend` (an extension's, outside a directory's documents) too, and `ResumeTx` lifts it once they
  are active. Also the console's people, where a server has any (a self-hosted one has none unless
  `MAIL_PASSWORD_SIGN_IN` is on): users (instance
  roles `owner`/`member`; active, disabled here, or suspended by an extension (`Suspend`/`Resume`,
  `suspended_at`, migration 0018: `status = 'disabled'` with the grants kept, unusable, until a
  resume; a disable here clears it and stands over it); a password the server never receives: the
  key scheme's ceremonies, `accountkeys.go`, store an auth verifier, the account public key written
  once, the password and recovery wraps, the target salt and parameters; an old server-side password hash of a person who
  never enrolled is checked by nothing, and a reset invitation clears it), sessions (a 43-character opaque token stored as SHA-256, 14 days at most,
  never extended, with a step-up time; signing out everywhere, `EndAllSessions`, records its moment
  in the same write, `users.signed_out_everywhere_at`, which nothing else that ends sessions does),
  the ceremonies' single-use tickets and reset invitations (`CreateReset`, which nothing calls since
  the CLI's `user password` went: the routes complete one an earlier release issued),
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
  too), `Keys.RevokeCreatedBeforeTx`; `SignOutEverywhereFor` is a person's sign-out everywhere asked
  at a moment (a request an extension carries, recorded in its write): their live sessions started
  by its second ended and counted, their tickets and the links they started by then gone, the
  moment recorded as their last sign-out everywhere, never moved back (one ahead of now is now);
  and `ExternalSignIn.PersonCreatedTx`, run in the transaction
  that creates a new person, right after their identity is linked, which rolls it all back on an
  error. A person acting through an extension (`KindPerson`, built only by `service.ActFor`, never
  by a transport) is a person with no session and no key: `IsKey`, `IsInstance` and
  `IsWorkspaceKey` are false for it, `IsPerson` and `Actor` name them. `authtest` creates cheap
  users and keys for tests.
- `internal/ratelimit` — token buckets per address (IPv6 per /64) and per key prefix (an issued
  key's selector: `Service.CredentialSubject`). An
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
  token sources. Removing a mailbox keeps it 30 days (`removal.go`; `removed_at`, `purge_after`),
  ending its pause first (`store.EndPauseTx`), as every deletion for good does:
  `Get` and `Visibility` leave it out, so nothing finds, dials or refreshes it (`Registry.Mailbox`
  builds one only through `Get`, and caches none built while a removal let go of it), and only
  `GetRemoved`/`ListRemoved` read it; what keys held on it and its consent attempts go at once. A
  recovery clears the removal and pauses its sync (`sync_paused_at`, by whoever recovered it; no
  notice, its `recovered` entry the record) until someone who may resumes it, where its consent
  stands (`store.ConsentStandsSQL`: a personal one's, its person's), with whatever index it kept,
  and off otherwise, and keeps the hold the operator's turn-off put on a personal one
  (`sync_held_at`; migration 0025 made the recoveries' holds it could tell apart, by the code or
  the journal, what a recovery does now). The hourly sweep, at start too, deletes it for good past its 30 days, never early, a closed
  team's included (`PurgeExpired`); a link that never connected (no credentials) is deleted at
  once, and so are the removed mailboxes that go with a person closed, their journal entries with
  them (a never-connected link in a workspace from another source is journaled `purged` instead,
  its earlier entries kept, for an extension to report). Its address stays taken in its workspace
  meanwhile (`ErrRemovedDuplicate`). One Gmail account is one mailbox per workspace under every
  spelling of its address (`address.go`, `canonicalAddress`: case, and at `gmail.com` or
  `googlemail.com` the dots, a `+tag` and the domain; a Workspace domain's by case only), live or
  removed, asked before anything is dialled and again in the inserting transaction
  (`duplicateOfTx`); rows linked before the rule stay. A Google mailbox deleted for good — the
  sweep, its owners or the operator early, the person it goes with deleted or forgotten — has its
  grant revoked at Google (`revoke.go`, `GoogleRevokeURL`, which the daemon and `user delete
  --bootstrap` set): the refresh token opened inside the deleting transaction, revoked after the
  commit, best effort, bounded, logged without it, never while another mailbox here holds the same
  grant (the same canonical address, compared in Go, through any client: Google keeps one grant per
  account and Cloud project; one Workspace account under two addresses is not recognised, which
  costs an early re-authorization, never access); Microsoft has
  no such endpoint and a password nothing to revoke. A link at the provider (`links.go`, migration
  0026's `oauth_links`; Gmail and Microsoft, the web client only) names no address: it asks for
  `openid email` beside the mailbox's scopes, with the account chooser (Google's
  `prompt=select_account consent`) and no hint, holds only its state, PKCE verifier and what was
  asked until its return (10 minutes, kept 10 more for its starter to read how it ended), at most
  `MaxLinksWaiting` (5) waiting per person, a new one replacing theirs for the same provider and
  workspace; it learns the address from the token endpoint's ID token (`identity.go`: audience,
  issuer, Microsoft's tenant, times, Google's verification, and a subject, `sub`, present, at most
  255 bytes, visible ASCII; no signature, as OIDC allows there; every refusal `ErrNoAddress`
  naming only its check; `identityOf` returns the `sub` and Microsoft's `tid`, lowercased, beside
  the address, `linkIdentity`, for the trial codes, kept, logged and put in an error nowhere, and
  printed, logged or encoded as `[link identity]`; `finishLink` keeps the address alone; nothing
  else of the token kept) once its
  return took it (`taken_at`, read in the take's transaction, which waits for the writer
  `LinkTakeTimeout`, 5 s, at most: `ErrLinkBusy`, nothing taken; its finish bounded from then),
  refuses a mailbox the workspace holds, proves the grant, and creates the mailbox with its key and
  its linker's grant in one transaction that finishes the link (`ErrFlowExpired` if it ended meanwhile,
  `store.EndLinksTx`, which runs wherever its starter's consent attempts end, and on a reset, a
  recovery and a sign-out everywhere). A finished link keeps no verifier, a connected one no
  address; a failed one its reason and the address named (a duplicate's only while the workspace
  still holds a mailbox under it), which a purge of that mailbox deletes.
  Re-authorizing keeps the mailbox's scopes and `login_hint`. What a login found the IMAP server to
  be (`ServerKind`, `ServerKindOf`: the send path's `provider.IsGmailServer`) is kept with the
  mailbox (`accounts.server_kind`, migration 0027): a password mailbox's login check records it
  when it is added, and every sync login when it changes (`SetServerKind`); only a report reads it.
- `internal/workspace` — workspaces (personal, team, the one operator workspace), members and
  per-mailbox grants (`read`, `act`, `send`, `manage`, which owners and admins hold by their role
  and members only as a stored flag), the protections each write keeps (last owner, last reader)
  and the workspace `Source` (local, or `Platform`, which mirrors an external directory: nothing of
  workspaces or memberships changes here, a person created gets no workspace from it, and the
  directory's documents send every one through `mirror.go`). A workspace's kind, source and
  person never change (migration 0016's trigger); a team closes and reopens (`SetClosed`,
  `closed_at`, migration 0019), and nothing in it is used meanwhile. A removed mailbox's grants
  take one change, flags taken away (`Revoke`, `keptMailboxTx`; the schema refuses giving anything
  on it), its last kept reader keeping read; the membership's status is never asked of a revoke.
  Only a local team goes with its only member
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
  reads, would read once resumed or would read but for a hold, and nobody else does or would, a
  removed one as once it is recovered, until it is deleted for good.
  `snapshot.go` reads who holds what on a workspace's mailboxes for an extension (`SnapshotTx`, in
  one read transaction): every mailbox, live and removed until deleted for good, its pause, label
  and masked hint, never its address; each person with a flag or a sealed grant, whether they read,
  wait, are enrolled and hold the key at its epoch; the people a team mailbox cannot lose. It
  decides nothing. `settings.go` reads, beside it and with bounds of its own, a workspace's
  settings (`SettingsTx`): each mailbox's row (provider, sign-in, state and reason, first
  connection, attribution, key epoch, sent copy and server, consent, agreement, pause, hold, index
  and last sync, removal and whether any index is kept) and each member's status, never an
  address. See `docs/workspaces.md`.
- `internal/sync` — one worker per account, three sessions of fixed role (`idle`, `sync`,
  `interactive`); a paused mailbox has no worker and keeps only its interactive session, logged in
  between calls as a worker's is (`store.PausedAccounts`). Every sync login records what the
  server is when it changed (`account.Repository.SetServerKind`).
- `internal/service` — the use cases and all authorization, access to mailboxes included; a
  concrete struct, not an interface. Whether anybody signs in is decided here (`external.go`):
  `passwordsInUse` refuses every route that signs in with a password, signs up, recovers or resets
  a password, changes a password or a recovery code, steps up, or creates or accepts an invitation,
  `errExternalSignInOnly` first under `Deps.ExternalSignInOnly`, else `errNoPeople`
  (`not_authorized`, "this server has no people: …") without `Deps.PasswordSignIn`; `hasPeople`
  (either one) is what lets `Authenticate` look a session up at all, and `Recheck` keep one: on a
  server with no people a session is `auth.ErrInvalidSession`, the `401` of an unknown token, never
  looked up. API keys are untouched by either. No route says where MCP is (`GET /v1/me/mcp` and
  `MCPAccess` went); a listed key's `sends` folds in `MAIL_KEYS_MAY_SEND`. `issuedkeys.go`: keys an issuer makes (`KeyIssuer`: `Claim`,
  `Verify`, `Used`, never blocking), the bearer read in one place for every transport (`Bearer`:
  one value, `Bearer` in any case, one space, at most 4096 bytes, never trimmed), a credential the
  issuer claims checked first and never reaching own keys or sessions, a session's shape never
  offered to it, one `401` for every refusal of one (`BearerRefused` for a malformed header that
  carries one), the rate-limit subject (`CredentialSubject`), a use refused reported only for a
  live mailbox of the key's own workspace (`UseRefusals`), and `RevokeOwnWorkspaceKeys`. An issued
  key reaches personal workspaces alone, never sends, and acts only under its creator's actions
  consent whatever `MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT` says. `issuedapply.go`: an issuer's page
  of records and its cursor in one write (`ApplyIssuedKeys`, `IssuedKeysPage`, `IssuedKey`,
  `IssuedGrant`; conflict `ErrDirectoryMoved`, `ErrNotKeyIssuer`), each record in its own savepoint,
  one the schema refuses applied again partial (only what narrows), and one still refused stopped
  (the operator's stop, `Faults`), so no record fails its page; zero times as the issuer means them
  (a revocation at a time unknown is now, a zero expiry the earliest); per record: a selector
  forgotten never stored again, a key never held that arrives ended only its stub, a workspace not
  held, a team or another creator refused (`IssuedRefusals`), the creator's belt (keys created
  before the directory's marks revoked, microseconds, no margin, at every storing and in the
  document's own write) and stop, terms judged once, grants judged once (read only from a giver who
  reads the mailbox here, waiting for a giver not followed since the reset and judged in their
  document's write, act only beside read, read leaving taking act in the same statement, never
  send; a partial record's grant no row can hold refused with none); a key's end the earliest of
  what ends it, and what is owed of it gone with it by selector or id; a reset never widens and its
  pass owes what is held narrower (kept to what is still kept off at every later write), its last
  page revoking what it did not list, never a key stopped for a record it named.
  `issuedreport.go`: what is owed (`IssuedKeysReport`, `IssuedKeysTold`), uses per
  day (`RecordIssuedKeyUses`, `IssuedKeyUsage`, `IssuedKeyUsesReported`), the stops reported, one
  per key and cause, the first stopped first (`IssuedStopCauses`: `operator`, `creator`,
  `withdrawn` under `IssuedKeysRefused` in place of an operator's hold, never stored),
  the operator's hold (`HoldIssuedKey`, `ReleaseIssuedKey`, an unrestricted instance admin key;
  `StopIssuedKey` for an extension), `EndExpiredIssuedKeys` (a key's end at its expiry, the next
  one answered, and open requests woken) and `SweepIssuedKeys` (one key per write, the periods of
  `docs/workspaces.md`). `AccessSnapshot` lists each mailbox's live issued keys
  (`SnapshotMailbox.Keys`), never among its people. `AccessChanged` is closed at every
  `accessChanged`, and taken before every check of a caller, never after: event streams, the long
  poll and a download's `Download.Still` (checked before the first byte and between chunks) stop
  within a second of the write that took a caller's access; a message's body and an inline
  attachment are checked for read again once the mail server was read. `directory.go` is what an
  extension that follows an external
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
  recovered, its kept last readers protected, until it is deleted for good). `settings.go` is what
  it reports beside it: a followed workspace's snapshot and settings in one read
  (`WorkspaceReport`: availability, who may link, the team sync text, each mailbox from fixed lists
  — a reason matched exactly on `account.Reason*`, a grant the provider ended (`grant_ended`) by
  `account.GrantEnded`, `filesSentCopy` beside the send path's rule —
  and each member, each list with its own bound, the settings read even when the snapshot is too
  large), a person's own settings (`PersonSettings`: consents against the texts accepted now,
  enrolment, the last sign-out everywhere, notices from the journal's label and hint, never the
  notice API's address, the actor known only while a member) and which people changed
  (`PersonChanges`); the lists it says (`MailboxReasons`, `MemberStatuses` and the rest) are what an
  extension maps one by one. `links.go`: a link at
  the provider for a person signed in (`StartLink`, `GetLink`, its starter's alone, and the return
  `CompleteOAuth` sends there when no consent of the caller's has its state), which names no address
  and carries no key at its start; its return with a code carries the mailbox's key and is checked,
  read only and within 20 s, before its code is taken (who may link, the freshness hook for a team,
  a step-up still fresh once the finish is over, `auth.RequireStepUpFor`), the transaction that
  creates the mailbox asking again; a return carrying the provider's refusal is recorded with no key
  and no step-up, and a mailbox the workspace holds, live or removed, is one `conflict`.
  `freshness.go`: the
  hook (`Deps.Freshness`) asked before thirteen acts of a person signed in — a key sealed to
  someone, flags raised for a person or a key, a key created, a link into a team (a link at the
  provider's at its start and again at its return), a team mailbox's
  consent started again, its sync switched, paused or resumed, an action on it or a send from it, and, by an owner, a
  removed team mailbox deleted for good (`FreshPurge`) or its sync turned off
  (`FreshRemovedSyncOff`) — after the read-only authorization (a team withheld or closed refused
  first) and outside every write; the write,
  or for those that write nothing first an immediate re-check, asks again who may, so an answer
  that ended the actor's place stops the act. Never asked for reading, removing, recovering, taking
  access away, a personal mailbox deleted for good or its kept index turned off, the sweep, the
  operator's acts, or a key's own acts, nor for renaming a mailbox (`RenameAccount`, `PATCH
  /v1/accounts/{id}`, and a person's rename through an extension: a name widens no access).
  A person acting through an extension is asked about as one signed in is. `delegated.go`:
  `ActFor(Delegation)` is the one way for an extension to run a person's request with no session
  (`PersonActs`: sync now, pause, resume, lift hold, team sync on and off, rename (refused
  `state_changed` in its write when the label it was drawn from is no longer the mailbox's; beside
  it `RenamedLabel`, the label a snapshot would report once renamed, read and refused as the rename
  authorizes it, writing nothing, never the address), remove, recover,
  purge, a removed mailbox's kept index off, raise and drop flags, remove access, agree to and
  withdraw a consent, sign out everywhere before a moment, dismiss notices in a batch), each the
  use case a session runs through the unexported act its method calls, with the request recorded
  in its write (`store.Request`): once only, a no-op recorded, its item in the request's
  workspace, a closed team `team_closed`, a narrowing refused only for a widening at or after its
  first second,
  a widening stamped with it, an error after the commit answered from the record. To whoever may:
  a recovery of a mailbox live again is done, of one the workspace's journal shows deleted for
  good `recovery_over` (both as its write reads it too); a team's turn-off of a mailbox removed
  since (as its write reads it) deletes its kept index, an owner's; a removed mailbox's kept index
  off never reaches one recovered since (done), nor one removed again since (`set_since`), and asks
  the freshness hook for a team only; a deletion for good of one removed again since is
  `set_since` too; taking flags away or a drop of read asks it nothing.
  `SweepRequests` is the hourly housekeeping's (`internal/app`). Every exported
  method that takes a principal refuses `KindPerson` first (`refuseDelegated`,
  `ErrNeedsOwnSession`), held by a test that calls each by reflection, and so do the key and
  session sides inside the acts (`requireSession`). `refusals.go`: `RefusalOf`, one word of a
  closed list for an act's refusal, read from the sentinel its `*Error` carries
  (`ErrActorInactive` and `ErrFreshUnconfirmed` are also what the freshness hook carries).
  `GrantRequest.From`: a grant change drawn from what someone held, refused (`conflict`) unless the
  grant holds exactly that in the write. `storage.go`: `GET /v1/me/storage` by usage rule 1
  (`rule_version`, `bytes`, `provider_bytes`), the live mailboxes the caller reads and the removed
  ones they list on the removed mailboxes' own paths (`removedToCount`: a personal workspace's
  person, a team's active owners of an open team not withheld, the operator for the operator
  workspace's), each `removed` until deleted for good; `database_bytes` the operator's alone. The
  refusals of a tool's credential (`errToolNeedsKey`, `errKeyNotAgreed`) name no console.
- `internal/api` — REST `/v1/...`, SSE and the long poll, each checking its caller again at
  `Service.AccessChanged`, and downloads checking before the first byte and between chunks, refused
  or cut off (never ended whole) once the caller may no longer read; the operator's hold of an
  issued key (`PUT|DELETE /v1/issued-keys/{selector}/hold`); a link at the provider's routes sit beside
  `POST /v1/accounts/oauth/callback` (`/v1/accounts/oauth/links[/{id}]`, never under
  `/v1/accounts/{id}`, whose GETs one would overlap), and the callback waits 70 s, a link's checks,
  its take and its finish. The API's own `/.well-known/oauth-*` mounts went with MCP (both a 404).
  `internal/mcp` — tools, resources and the transports (stateful Streamable HTTP, `HTTPHandler`;
  stdio, `RunStdio`), kept with its tests as a library **nothing in this repository mounts**: a
  self-hosted server serves no MCP of its own (platform decision 0065), and no binary built here
  links it or the go-sdk. It stays for a later binary that serves MCP over the same service (MCP
  through the hosted service, for a connected server). One `sdk.Server` per session, for the key
  that opened it, with the same tool definitions for the whole process (the SDK's `SchemaCache`
  keeps each schema by pointer, forever), and an event store of its own bounded in bytes and age.
  Both are thin adapters over `internal/service` (`AuthenticateTool` decides what a tool reaches).
  See `docs/mcp.md`.
- `internal/webui` — serves an edition's built page from `MAIL_WEB_DIR` (via `os.Root`), mounted
  last (a self-hosted server sets none: the API only, `GET /` a 404);
  a JSON 404 for unknown `/v1/`, `/mcp` and `/.well-known/` paths (`NotFound`, which the core
  also mounts at `/mcp` itself, a console or none); what an embedding binary adds
  to its page documents and beside them is `Pages` (`pages.go`, `NewWith`), which never weakens
  the page's own headers. It is also a transport for depguard.
- `web/` — the console's **core**: Vue 3 + TS, with no router, Pinia or UI library, which an
  edition compiles from source; it ships no edition, no entry and no page, and nothing here builds
  or serves one (a self-hosted server has no web console, platform decision 0063; the open edition,
  `web/src/open`, `main.ts`, `index.html` and `App.vue`, was deleted with the panels only it drew).
  A Node project of its own: `go.mod` ignores it, `make check` does not touch it, `vitest.config.ts`
  runs its specs, which run under an edition of their own (`web/test/edition.ts`: placeholder
  texts under the daemon's default revisions, a test console on `ConsoleShell`), imported by
  nothing under `web/src`. The edition (the hosted service's app, private) compiles `web/src`;
  nothing under `web/src` imports from outside it, and nothing under `web/src` or `web/public`
  names the hosted service (`web/test/editions.spec.ts`, `web/test/hosted.ts`). The core's
  catalogs hold only the core's own words. An edition may draw the core's panels in a frame of its
  own (`useConsoleFrame`, `useAppLifecycle`, `AppOverlays`, `.console-surface`,
  `requestWorkspace`); a root of its own that wants the page held still calls `useFixedFrame`
  (`html.fixed-frame`), which no file of the core does. See `docs/console.md`, "Editions".
- `deploy/` — the self-hosting packaging (`docs/self-hosting.md`), API only: `Dockerfile` (Go
  builds a static binary onto distroless `static-debian13:nonroot`, with no console and no
  `MAIL_WEB_DIR`; the build context is the root, where `.dockerignore` is an allowlist of `go.mod`,
  `go.sum`, `cmd/` and `internal/`), `compose.yaml` (`MAIL_WEB_DIR` kept empty, read-only, no
  capabilities, the port on loopback, a named volume, a fixed subnet whose gateway is the
  documented `MAIL_TRUSTED_PROXIES`) and example systemd units (`mailie.service`,
  hardened, no `MAIL_WEB_DIR`; `mailie-backup.service` and its timer). Base images are pinned by
  digest: a Go bump (go.mod's toolchain, CI's `GO_VERSION`) moves the Dockerfile's stage with it.
  `make image` builds `mailie:local`.
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
./bin/mailserver apikey create --bootstrap --scope admin --name cli   # no daemon, like every --bootstrap
export MAIL_ADMIN_KEY=<the printed key>
./bin/mailserver serve          # 127.0.0.1:8080, REST only: no people, no MCP
```

Then, beside the daemon: `./bin/mailserver account add --email you@icloud.com --password -` (the
password typed at a prompt, or piped), or `--email you@gmail.com` through the installed OAuth
client (`--paste` without a browser on this machine), and `./bin/mailserver account sync ID on`:
nothing is indexed before. Tools use REST with an instance key (`apikey create --scope read --name
tool [--accounts IDS]`, through the daemon with `MAIL_ADMIN_API=true`, or `--bootstrap` with it
stopped). There is no console to run, nobody to invite and no MCP to connect: a self-hosted server
has none (`MAIL_PASSWORD_SIGN_IN` stays unset; it is for an edition with a page where people sign in,
such as the hosted service's `make cloud-run`). Details, and registering the OAuth clients, in
`README.md` and `docs/self-hosting.md`; the REST reference is `docs/console.md`.

## How to test

- `make check` = fmt + vet + lint-layout + `go test -race ./...`. All offline: an in-process IMAP
  server, a fake `go-smtp` server, `httptest` for OAuth, SQLite in `t.TempDir()`, MCP transports in
  memory (`internal/mcp`'s own tests: nothing else mounts it). A test that signs people in with a
  password turns `PasswordSignIn` on (the service fixture and the API harness default to people;
  `noPeople` turns them off). `make lint` runs golangci-lint with the integration tag.
- `make it` starts `it/compose.yml` (Dovecot 2.4.5, Mailpit) and runs `go test -tags integration`.
  It is the only place CONDSTORE, SPECIAL-USE and a real XOAUTH2 run; CI runs it, it is not
  optional.
- The console's core: `make web-check` (typecheck and vitest, no build), outside `make check`; CI
  runs it in a job of its own on Node 24. The contract fixtures in
  `internal/api/testdata/contract/` are written by the real handlers (`go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`) and
  read by `web/test/contract.spec.ts`; never edit them by hand or create them from the web side.
- `make public-source` exports the public snapshot; run it before a pull request.
- `make image` builds the self-hosting image (Docker, outside `make check`); CI's `image` job builds
  it through `deploy/compose.yaml`, starts it with a throwaway key and checks `/v1/healthz`, a 404
  under `/v1`, `POST /mcp`'s 404 (no MCP), `GET /`'s 404 (no console) and a clean stop.
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
  appears on both sides, it is in the wrong place. It holds for `internal/mcp` though nothing mounts
  it: the library stays a transport, so a binary that serves it later serves the service's rules.
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
  good, turns sync off, pauses or resumes a team mailbox's sync, or strands, and is not a record of `events`, which a deletion for good takes
  with the mailbox. It is an extension's outbox, read per workspace from a cursor; which workspaces
  from another source changed is marked by triggers in the same transaction (`workspace_changes`).
  A notice never goes to whoever acted, and a stream says `event: notice` (no `id`) only after the
  commit that gave one.
- **Secrets.** Never in plain text in the database, in logs (`obs` redacts) or in URLs. What must be
  opened again goes through a `secrets.Sealer`, with its binding and the caller's context: the
  credentials, the send-hash root (`meta.send_hash_root`, random, made at the first start, never
  derived from a key) and the salt key (`meta.kdf_salt_key`, K_salt of the key scheme, the same way,
  made and opened only where people sign in with a password here: `MAIL_PASSWORD_SIGN_IN` on and no
  `ExternalSignInOnly`), which the daemon refuses to start without opening (the salt key there
  only; elsewhere one a database holds is left unopened, and `rewrap-credentials` re-seals it when
  present). The sealer is the keyring
  (`MAIL_CREDENTIAL_KEY_HEX`) or AWS KMS (`MAIL_CREDENTIAL_KMS_KEY_ARN`, a key's full ARN, never an
  alias, with `MAIL_ENV` set; the keyring's keys then only open, and `MAIL_CREDENTIAL_SEALER=keyring`
  beside it is the way back, the KMS key only opening). The KMS provider takes the EC2 instance
  role's credentials through IMDSv2 only, unlike the backup's SDK default chain, and checks the key
  with `DescribeKey` before anything opens. `rewrap-credentials` opens the root and any salt key and
  re-seals them with the credentials under the active sealer; `--new-send-hash-root` and
  `--new-salt-key` replace one under another KMS key or `MAIL_ENV` only with `--kms-key-lost`. Only an envelope that does not open (`secrets.DoesNotOpen`)
  is a lost key: nothing replaces, or tells the operator to replace, what a sealer could not try. TLS
  is mandatory for IMAP/SMTP; `AllowInsecureAuth` is set only by tests.
- **Console session = bearer, never a cookie.** The token goes only in `Authorization`, read by
  `service.Bearer` alone, never trimmed there; a credential an issuer claims (`service.KeyIssuer`)
  is checked first, against its keys only; any other is trimmed, and sessions and API keys are told
  apart by their shape (a key has a dot). Person routes (`/v1/auth/*`) refuse API keys. A session
  exists only where people do (`MAIL_PASSWORD_SIGN_IN`, or an extension's sign-in): on a server with
  no people every session token is the `401` of an unknown one, at authentication and every
  re-check, never looked up.
- **The server never receives a password** (`docs/key-scheme.md`, normative; its vectors are never
  edited by hand). Its password half is the core's code, used only where `MAIL_PASSWORD_SIGN_IN` is
  on (a self-hosted server has no people by default, platform decision 0065; the hosted service
  signs people in through id.); taking it out of the scheme is a new spec version, under 0034's
  review, never an edit. A sign-in proves an auth key the browser derived under the salt and parameters
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
  checked again in the transaction that writes (a link at the provider's, whose key goes with its
  return, also before its code is taken, still fresh once its finish is over; a sign-in counts, the hosted one is id.'s
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
  reset invitation, which nothing issues now (an earlier release's is still completed). New passwords have at least twelve code points. The console's half
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
  from any owner or admin (`act` to a reader), stored `manage` to members only; an admin never
  gives, takes or removes an owner's flags (platform decision 0060, `ownerAccessTx`: before the
  freshness hook, and again in the write with the target's role then), owners change each other's,
  and supplying a waiting owner the key is no flag change. Renaming a mailbox is `manage`'s
  (`PATCH /v1/accounts/{id}`, or `PersonActs.Rename` through an extension, a name it holds already
  recorded as a no-op; never a key's, never a removed one's), under one name rule for a rename, an
  add and a link (`checkMailboxName`: trimmed, 200 characters, no control character).
  A team mailbox
  someone reads always keeps a reader (keys and roles never count). An API key belongs to its
  workspace: only its owners and admins, signed in, create, list and revoke keys (a key never
  mints one; an edition with `KeysInPersonalWorkspaceOnly` creates none in a team, and lets none
  reach a team's mailbox; one with `KeysIssuedElsewhere` creates none and widens none, and a key
  an issuer made reaches a personal workspace's alone, never sends, acts only while its creator
  allows actions whatever the switch, and is changed by its issuer alone); a key reaches
  exactly what it holds in `key_access`, read live — `read` given only by an owner or an admin who
  reads that mailbox then, `act` and `send` by any of them — and keeps it
  whoever gave it; it never counts as a reader nor passes `read`, and is revoked when its creator
  leaves the workspace or is disabled, suspended or deleted (not on a demotion), and in every
  workspace when they recover their account or complete a reset (`account_recovered`, in that
  transaction; a password change and signing out everywhere keep it). A key an issuer made differs:
  a disable here holds it (the `creator` stop) and never revokes it; a suspension outside the
  directory's documents holds it until the resume; the directory's marks revoke only the ones
  created before them, at microseconds with no margin, whichever arrives first; the operator holds
  one by selector, each stop lifted on its own; a deletion takes it with the personal workspace and
  leaves its stub (selector, how, when) for a year after its creation. A suspended person
  reads nothing, waits for no key, is supplied none and counts in no protection for anyone else
  (closing them asks it as for anyone active, counting what they would read once resumed); their
  grants persist for a resume, which restores no session and no key. A closed team (migration 0019) is a
  gate on use, never the reading rule (`store.OpenWorkspaceSQL` beside `ReaderSQL` in visibility,
  access, what a key holds and sync): its mailboxes reach nobody, its owners and keys included
  (`not_found`), and nothing in it changes, by anyone, until it reopens (`conflict`; the schema
  refuses what would give it something); its grants and keys are kept. Only the operator closes a
  local team; a team from another source closes where it is managed, through an extension. A
  removed mailbox (kept 30 days, `docs/workspaces.md`, "Removed mailboxes") is `not_found` to
  everyone, the operator included, but on the removed mailboxes' own paths; it has no reader for
  any use (`store.LiveSQL`, inside `ReaderSQL` and `FlagHolderSQL`) and nothing is given on it (the
  schema refuses), but it keeps protecting until it is deleted for good: whoever would read it once
  recovered (`store.KeptReaderSQL` and the other `Kept` fragments, `workspace/reader.go`) counts in
  the last-reader protection, the refusals of closing a person and of a reset, and what a change
  strands, as a reader of a live one does; deleting it for good (the sweep, its owners, the
  operator, a person's closure) ends that with its grants. The one trace of it outside its own
  paths is its id in a team member's `last_reader_of`, which the team's admins read too, so the
  protection is said before it refuses: nothing else of it reaches them. Only its owners remove it — a personal mailbox's person, a team's active
  owners (an admin, only a link of their own that never connected), the operator for an operator
  mailbox — and they alone list, recover and delete it for good; the operator also lists and
  deletes any workspace's for good, and recovers no person's or team's. A team's active owners
  alone also take flags or access away on it (`RevokeAccess`, as on a live one, the member in any
  standing; removed as its write reads it, one removed since it was read included), its last kept
  reader keeping read (`last_reader`), and nobody gives anything on it.
  A recovery brings back the readers who still qualify and nothing a key held; none in a closed
  team, nor of a team mailbox nobody who could read it is left to read. **The directory
  exceptions**, under a source that
  mirrors an external directory (`docs/workspaces.md`, "Following an external directory"): what
  the directory says applies in a closed team too (memberships change while it is closed); its
  end of a membership, or of a person (`ForgetExternalPerson`), takes no refusal for the last
  owner, the last reader or a person who is not active, and may leave a team mailbox, live or
  removed and kept, with no reader, journaled `stranded`; a membership it ends while its generation is unconfirmed, and
  whose end would strand a mailbox, is held (disabled, reading nothing, its flags and grants kept)
  until a document lists it again or the generation is confirmed; and a document whose workspace
  part is refused still applies what it revoked. A disable here stands over a directory: its
  documents never resume a person disabled here, nor one whose disable they do not know of. The
  freshness hook gates and never authorizes: every act it is asked about keeps its own checks, run
  again in the write. A person acting through an extension (`ActFor`, `auth.KindPerson`) reaches
  what they reach signed in, through those acts alone, each run once, never past a step-up and
  never writing a key or a grant; a narrowing they asked never takes away what was given at or
  after the second they asked it.
- **An OAuth flow belongs to whoever started it.** Its owner is checked in the same statement that
  takes the row, `oauth_pending`'s `DELETE` or `oauth_links`' `UPDATE` (`taken_at`), **before** the
  code is exchanged; another owner's is `not_found` and the row stays. The web flow's redirect is `MAIL_PUBLIC_URL + /oauth/return` (a
  console route), never derived from `Host`; a console that is not on loopback is never offered
  `loopback`. Refresh uses the client that issued the token (`accounts.oauth_client`);
  `invalid_client` is a configuration error, not a dead grant. A new grant, a first one or a
  re-authorization's, is stored only once one login with it worked (`checkGrant`): a refusal is
  `ErrMailboxRefused`, and a check that could not run (a server down, slow or throttling) is
  `ErrGrantUnproved`, `internal` "upstream: …" with a `Retry` of 30 s, storing nothing either;
  the mailbox keeps the grant it had.
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
  person withdraws or turns it off (a removed personal mailbox's kept index alone, by the route a
  team's owner uses, `PUT /v1/accounts/{id}/sync` with `enabled: false`: their consent, its hold
  and its removal kept, nothing journaled, and a recovery then brings it back paused with nothing
  kept; one recovered since they asked is `conflict`, `ErrNotRemoved`, its index kept), a removed
  team mailbox's owners turn it off or the consent still bound to its linker
  stops; with `removed_only: true` that route turns off only a removed mailbox's kept index, and
  one recovered since is `conflict` (`ErrNotRemoved`), nothing of it turned off. An agreement given through an extension (`ActFor`) is recorded as on the screens, its
  version, its person and when they asked (the request's first moment, or a later moment an
  agreement that stands holds, which it keeps); the request's id lives
  only in its record (`extension_requests`), never on the agreement. The operator turns any
  mailbox's sync off (`StopMailboxSync`,
  `POST /v1/accounts/{id}/sync/stop`, `account sync ID off`), in any workspace, live or removed,
  a closed or withheld team's included: its index deleted in that transaction, a team's consent
  withdrawn (its owners and admins give it again), a personal one held (`sync_held_at`), live or removed, until
  its person turns it on, a recovery keeping that hold, its readers told and journaled with the actor `operator`
  (`store.ActorOperator`) whenever it could sync or kept an index it deletes (a recovered personal
  one, paused, included), never twice for nothing. A recovered one comes back paused
  (`sync_paused_at`) where its consent stands, off otherwise, its index usable at once, and syncs
  nothing until someone who may resumes it: its person, a team's owners and admins at the current
  sync text (a resume records the team's agreement again), the operator. A team consent migration 0011
  copied from its linker (`sync_enabled_via = 'migration'`) stays bound to them until confirmed:
  their withdrawal, disable or deletion stops it and deletes its index, telling its other readers
  (closing them is refused without force while someone else reads it, or would once it is
  recovered); their suspension
  pauses it, as it pauses their personal mailboxes, index kept. A closed team's mailboxes sync
  nothing, their index and consent kept, until it reopens. A team mailbox
  nobody can read is never switched on. Closing a person without force is refused for the last
  reader of a team that outlives them (one another member remains in, whatever their status, or
  one whose source is not local), the same test as whether the team goes with them. Actions and sending have their own consents, the actor's own,
  checked again right before the mail server is touched; a withdrawal of either writes its moment
  (`actions_consent_withdrawn_at`, `send_consent_withdrawn_at`, migration 0024) in the `UPDATE`
  that clears it and a grant clears that in its own, answered as `withdrawn_at`, never beside a
  consent, so an edition may ask only someone who never answered, or ask again; a workspace key's are the key terms its
  creator agreed to (recorded on the key), with the key, its flag and its scope checked again
  instead, so no text says a person's own switch stops a key created since 0012. The revisions come
  from `MAIL_CONSENT_VERSION_*`, whose defaults name the deleted open console's last texts (the git
  history keeps them), pinned in Go alone (`internal/config/consent.go`, `consent_test.go`) and
  named by the core's specs (`web/test/edition.ts`) and the contract fixtures; an operator mailbox
  records none. **A pause is not a consent.** A personal mailbox's person, a team
  mailbox's owners and admins by their role (freshness `FreshTeamSync`), and the operator an
  operator mailbox's, pause one mailbox's sync (`POST …/sync/pause`; `sync_paused_at`,
  `sync_paused_by`, migration 0025) while its consent stands and nothing holds it, where the
  edition offers it (`Options.SyncPauseOffered`); a recovery pauses it too. The engine syncs
  nothing of it (`SyncEligible`, the engine's rule), and its index stays, read, searched and
  followed by actions (`IndexFollows`, the index rule), with no time limit. The same people resume
  it (`POST …/sync/resume`, in every edition); a team mailbox's resume names the current sync text
  and records the team's agreement to it, by the resumer, now, as turning it on does, and needs a
  reader; turning a team mailbox on records the agreement and leaves a pause. A resume, every end
  of its consent (a withdrawal, a turn-off, the operator's, a bound consent stopped), a removal
  and a deletion for good end it, withdrawing its notices; a paused mailbox's consent always
  stands (the schema for a team's or an operator mailbox, the code for a personal one), the schema
  keeps a removed or held mailbox unpaused and a hold on personal mailboxes alone, and what a
  pause keeps may be nothing (a resume then indexes the last 90 days). Who paused it is forgotten
  with that person. Readers, grants and every protection ignore it. What the index has not caught
  up with while paused (a folder renumbered or resynced, an unknown send) waits for the resume
  (`errPausedBehind`, never `errMessageBusy`).
- **Errors.** Sentinels `errors.New("pkg: ...")`, always `errors.Is`/`errors.As`. The API answers
  `{code, message}` with seven fixed codes and never says which credential failed. What an
  extension calls keeps the cause of each refusal for `errors.Is` (`service.ErrNotFollowing`,
  `ErrDirectoryGeneration`, `ErrKindChanged`…); a new person under a directory's source without
  their document is `internal`, "upstream: …", with a `Retry` of 10 s; so is an act the freshness
  hook could not confirm (a `Retry` of 30 s, no reason on the wire), while a `*service.Error` the
  hook returns passes through as it is and a context's end is passed on. An act an extension runs
  for a person (`ActFor`) refuses with an `*Error` whose cause is one of `refusals.go`'s sentinels
  or a repository's own, read by `service.RefusalOf`; its code and message are a session's.
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
