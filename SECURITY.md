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
- The send-hash root, the random key of the hashes a send record keeps of the message it sent,
  sealed the same way: without it the database cannot confirm a guess of a message. And the salt
  key, the random key of the salt every address is answered for its password, sealed the same way.
- For synced mailboxes, an index of metadata: senders and recipients, subjects, dates, sizes,
  folders, flags and the names and types of parts. Message bodies and attachments are fetched on
  request and never stored.
- For each person, never their password: their browser derives an auth key from it (Argon2id,
  64 MiB), and the server keeps an Argon2id hash of the auth key, the person's account public key,
  their account key wrapped under the password and under a recovery code (which only their browser
  opens), and a hash of the recovery code's proof ([`docs/key-scheme.md`](docs/key-scheme.md)).
  People who signed up before the key scheme keep an Argon2id hash of their password until their
  next sign-in, the one time their password reaches the server again, which enrols them and clears
  it.
- Argon2id hashes of API keys, and SHA-256 hashes of session tokens, of the single-use tickets of
  the sign-in ceremonies, and of invitation and reset codes.

Whoever has both the database file and `MAIL_CREDENTIAL_KEY_HEX` can reach every connected mailbox.
Whoever runs the server can read the index: the account keys of this release protect no mail yet
(the threat model of the key scheme, [`docs/key-scheme-threat-model.md`](docs/key-scheme-threat-model.md),
says what they do and do not protect). A copy of the database lets its holder guess each enrolled
person's password at the cost of a 64 MiB Argon2id per guess, as the password wrap beside the
verifier is the same offline oracle.

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
- A console session is a bearer token, never a cookie. Signing in proves an auth key, never a
  password; a password wrap is answered only to an auth key verified in the same request and a
  recovery wrap only to a recovery proof, never to a session alone. Every way a sign-in can fail
  costs the same work and gets the same answer. Changing the password and replacing the recovery
  code each need the current auth key in the same request: a session alone, however recent its
  sign-in, sets neither. Giving "read" and writing a mailbox's keys will need the person's secret
  proved within the last ten minutes (a sign-in or a step-up) once mailboxes have keys, the key
  scheme's next step; until then access is given as before, without one.
- In the release that brings the key scheme only, a person who signed up before it sends their
  password in clear one last time, at their next sign-in, and a challenge says, to anyone, that such
  an address has an account not yet upgraded. Enrolment is one way: the server then refuses their
  password in clear, and the console never sends it for an address it remembers.

[`docs/architecture.md`](docs/architecture.md) lists the rules that keep these properties, and
[`docs/backup.md`](docs/backup.md) the threat model of backups.
