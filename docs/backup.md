# Backups

`mailserver backup` takes an encrypted copy of the database and uploads it to S3.
`mailserver backup restore` brings one back. Both are optional: a daemon without them runs as
before.

## What a backup does

`mailserver backup` runs beside the daemon, as a separate process (a systemd timer, say):

1. It opens `<MAIL_DATA_DIR>/mail.db` read-only (`mode=ro`), without the daemon's lock. The
   connection writes nothing, so it is safe beside a running daemon. It needs the daemon running:
   the `-wal` and `-shm` files must exist.
2. It takes a snapshot with `VACUUM INTO`, a single read transaction, into a private 0700
   directory, and runs `PRAGMA integrity_check` on it. A damaged snapshot is never uploaded.
   With the data directory read-only, a read can begin just as the daemon commits, and SQLite
   then refuses it with `SQLITE_READONLY_RECOVERY` (or, far more rarely,
   `SQLITE_READONLY_CANTINIT`); the snapshot starts over, up to five attempts in all.
3. It asks AWS KMS for a fresh AES-256 data key, encrypts the snapshot with it in 64 KiB chunks
   (AES-256-GCM, the `.mlbk` format), zeroes the key and deletes the snapshot. The data key's
   encryption context is
   `{"service": "mailie", "env": "<MAIL_ENV>", "purpose": "db-backup", "ref": "db/<name>.mlbk"}`.
4. It uploads `s3://<bucket>/db/<UTC time>-<8 hex>.mlbk` in one request with
   `If-None-Match: *`, so an existing backup is never replaced, and SSE-S3 at rest.
5. It logs one line: the object, the sizes, the sha256 of the encrypted file, the duration, the
   schema version and the row counts of three tables. Never any content.

The time in the name is the instant just before the snapshot: nothing committed before it is
missing from the backup.

## Configuration

Only these variables; none of the daemon's secrets, not even `MAIL_CREDENTIAL_KEY_HEX`. A
restore reads them too, except `MAIL_DATA_DIR`, and requires none of them (see "Restoring"):

| Variable | |
|---|---|
| `MAIL_DATA_DIR` | where `mail.db` is, as for the daemon |
| `MAIL_ENV` | `dev` (default) or `prod`; written into the encryption context |
| `MAIL_BACKUP_BUCKET` | the S3 bucket; required |
| `MAIL_BACKUP_KMS_KEY_ARN` | the KMS key's ARN, never an alias; required |
| `MAIL_BACKUP_REGION` | the region of the bucket and the key; required, no default |
| `MAIL_LOG_LEVEL` | optional, as for the daemon: `debug`, `info` (default), `warn` or `error` |
| `MAIL_LOG_FORMAT` | optional, as for the daemon: `json` (default with `MAIL_ENV=prod`) or `text` (default otherwise) |

An invalid value in any of them, the two log settings included, stops the command before it
touches the database or AWS.

There is no default region: where the copies of the database are kept is the operator's decision.
The key must be in `MAIL_BACKUP_REGION`.

AWS credentials never come from `MAIL_*` variables. The SDK's default chain finds them: on the
host, its own identity (an EC2 instance role through IMDSv2, for example); where a restore runs,
the signed-in principal's.

## Who can do what

The design asks the host for as little as possible, so that a compromised host can neither read
nor destroy the backups it made:

- **The host only encrypts and uploads.** Its identity needs `kms:GenerateDataKey` on the key,
  limited by condition to the context above (`service=mailie`, `purpose=db-backup`, and
  `env=prod` in production, with no other context key), and `s3:PutObject` under `db/` in the
  bucket. Nothing else: no list, read, delete or decrypt. A bucket policy should deny the host any
  `PutObject` without `If-None-Match: *`, so it cannot overwrite a backup whose name it knows.
- **A separate principal decrypts.** Only that principal may call `kms:Decrypt` on the key; KMS
  unwraps a data key only with its backup's exact encryption context, and CloudTrail records each
  `Decrypt` with the backup's `ref`.
- **Retention is the bucket's.** A lifecycle rule on `db/` expires backups. With no versioning,
  Object Lock or replication, nothing keeps a copy after that.
- **The credentials inside stay encrypted**, with the send-hash root, under the credential key in
  use when the backup was taken (`MAIL_CREDENTIAL_KEY_HEX`, or a key of
  `MAIL_CREDENTIAL_PREVIOUS_KEYS` whose rows had not been rewrapped yet), which is not in the
  backup. Keep those keys apart, and keep a key retired by a rotation for as long as any backup
  sealed under it is kept: `rewrap-credentials` re-seals the live database, never a backup. Without
  the key a restored database has the people and the index, but the daemon refuses to start until
  `mailserver rewrap-credentials --new-send-hash-root` has replaced the root, and every mailbox has
  to be authorized again. Under `MAIL_CREDENTIAL_KMS_KEY_ARN` they are sealed under data keys of
  the credentials' own KMS key instead, which open only on a server that key's policy lets decrypt,
  with the same `MAIL_ENV` ([`self-hosting.md`](self-hosting.md#credentials-under-aws-kms)).

Do not use SSE-KMS under the backup key: S3's own call to KMS adds the context `aws:s3:arn`, which
a key policy that admits only the four keys above refuses.

## Restoring

Run it where the decrypting principal signs in and where your data-protection rules allow a copy
of the database in clear:

```sh
mailserver backup restore --object s3://BUCKET/db/NAME.mlbk --kms-key-arn ARN --out restored.db
mailserver backup restore --file NAME.mlbk --kms-key-arn ARN --out restored.db
```

- `--kms-key-arn` is required (or `MAIL_BACKUP_KMS_KEY_ARN`), and only an ARN works. The ARN in a
  backup's header proves nothing until KMS has unwrapped the data key; without the pin, a backup
  sealed in another account under a key anyone may use would pass every check.
- The restore runs in the key's region. `MAIL_BACKUP_REGION`, when set, must agree with it.
- A backup whose context is not `service=mailie`, `purpose=db-backup` is refused before KMS is
  called. With `--object`, its `ref` must be the object it was fetched from.
- Every chunk is authenticated, in order, and `integrity_check` must pass; only then is the file
  linked into place. `--out` must not exist. A failure leaves no file; only a `SIGKILL` can leave a
  hidden `.mailie-restore-*.db` beside it.

Delete the restored file as soon as it has served: it is the database in clear.

### What a restore lacks

A restored database is the one in its backup: the database at the time in its name, or a moment
later. Every change made after that is missing, and deletions made after the backup are not
re-applied: people deleted and mailboxes removed since come back, with their credentials. Decide
what that means for your obligations before putting one into service.

### Putting a restored database into service

Stop the backup timer and then the daemon. Move `mail.db`, `mail.db-wal` and `mail.db-shm` aside
together, each if it exists (a stale `-wal` beside the restored file would be applied on top of it),
put the restored file in place as `mail.db` (mode 0600, owned by the daemon's user), and give the
daemon the credential keys that were in use when the backup was taken. With no rotation since, that
is the same `MAIL_CREDENTIAL_KEY_HEX`. After a rotation, keep the current key and id and add each
older key the backup needs to `MAIL_CREDENTIAL_PREVIOUS_KEYS`, with its id (`<id>:<hex>`): a
credential sealed under a key id the daemon is not given cannot be opened, and the daemon refuses
to start while the send-hash root is one of them. Then run `mailserver rewrap-credentials` while
the daemon is still stopped, so the restored credentials and root are sealed under the current key and the older one can leave the environment again, and start the
daemon, then the timer.
