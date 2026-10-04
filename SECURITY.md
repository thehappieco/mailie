# Security

## Reporting a vulnerability

Report vulnerabilities privately, through this repository's
[security advisories](https://github.com/thehappieco/mailie/security/advisories/new) ("Report a
vulnerability" under the Security tab). Do not open a public issue for anything that could expose
credentials, tokens or the content of messages.

Include the affected revision, a minimal reproduction with synthetic data, and the boundary you
expected to hold (which credential, which mailbox, which scope). Never include a real token, API key,
password, database or message: describe them instead.

## What the server holds

- One OAuth refresh token or password per mailbox, encrypted at rest with AES-256-GCM under
  `MAIL_CREDENTIAL_KEY_HEX`, with additional data that binds each value to its account and field.
- For synced mailboxes, an index of metadata: senders and recipients, subjects, dates, sizes,
  folders, flags and the names and types of parts. Message bodies and attachments are fetched on
  request and never stored.
- Argon2id hashes of passwords and API keys, and SHA-256 hashes of session tokens.

Whoever has both the database file and `MAIL_CREDENTIAL_KEY_HEX` can reach every connected mailbox.
Whoever runs the server can read the index.

## Threat model

- The credential key lives in the process environment and is readable by any process of the same
  user. That protects copies of the database (backups, disk images, a file moved elsewhere), not
  against code running as the daemon's user.
- The daemon listens on `127.0.0.1` by default and makes **no** exception for local callers: a local
  process still needs a key. Exposing it beyond the machine needs a TLS-terminating proxy in front.
- Keys are scoped (`read < write < send < admin`), may be restricted to mailboxes, expire within a
  year and are revoked by prefix. Revoked keys stay listed, because they answer "what could have
  read this mailbox".
- A person's mailbox is invisible to other people, owners included. Changing it or sending from it
  needs that person's own consent, checked again before every command that changes the mailbox and
  before every connection to the submission server. Instance keys belong to the operator and see
  every mailbox over REST.
- Mail servers are reached over TLS only, and an account host that resolves to a loopback, private,
  link-local or CGNAT address is refused at dial time unless the operator allows it.
- Logs redact addresses and credentials and carry ids rather than subjects or content.

[`docs/architecture.md`](docs/architecture.md) lists the rules that keep these properties, and
[`docs/backup.md`](docs/backup.md) the threat model of backups.
