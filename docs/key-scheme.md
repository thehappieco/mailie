# Mailie's key scheme: specification

- Spec version: 1 (phase 3, written before any server or console code that uses it).
- Status: **normative.** Sections 3 to 11, 13 and 14 fix every byte: they change only with a new
  version byte, a new label or a new kind, never in place, and the vectors of a published version
  keep passing. Section 12 is normative for what each side sends, checks and stores; the route
  names it gives are informative until the server serves them, when [`console.md`](console.md) and
  the contract fixtures name them. The server serves those of sections 12.1 to 12.6 under
  `/v1/auth/` (Appendix C); the upgrade of section 12.7 it served in the release that brought this
  scheme only.
- Built on: The Happie Co's kit, `github.com/thehappieco/kit` v0.7.0 in Go and
  `@thehappieco/kit` 0.7.0 in TypeScript. Its `SPEC.md` is cited as "kit §n". This document is
  Mailie's profile of it (kit §3): labels, headers, magic, kinds and additional data, and the
  rules around them. **It adds no cryptographic construction of its own.** Since v0.7.0 the kit
  publishes this profile itself (Go `profiles/mailie`, TypeScript
  `@thehappieco/kit/profiles/mailie`, kit Appendix D), frozen by this document's vectors, which it
  carries as `vectors/mailie/key-scheme-v1`; the core uses it, and keeps only what the kit leaves
  to Mailie (section 16).
- Vectors: `internal/keyscheme/testdata/*.json`, cited as `file#case-id`: 248 cases, 150 of which
  must fail (section 14).
- Code: Go [`internal/keyscheme`](../internal/keyscheme), TypeScript
  [`web/src/crypto/mailie.ts`](../web/src/crypto/mailie.ts) (section 16).
- Threat model: [`key-scheme-threat-model.md`](key-scheme-threat-model.md).

## 1. Scope

Phase 3 gives every person an **account key** and every mailbox a **key pair**, and lets a person
read a mailbox only while they hold a **grant**: the mailbox's private key sealed to their account
key. This document fixes:

- the account key, an X25519 key pair made in the person's browser (section 4);
- how the account key is kept: under a zero-knowledge password and a recovery code on a
  self-hosted server (section 5), under a key derived from the product key that the platform's
  identity provider, id., delivers on the hosted service (section 6), and at rest in the browser
  (section 7);
- the mailbox key pairs and their epochs (section 8), the grants (section 9), the envelope domain
  and kinds they are sealed under (section 10);
- the step-up that giving access and writing keys ask for (section 11), and the ceremonies
  (section 12): signing up, signing in, recovery, the reset, the upgrade of existing accounts
  (served in one release, history since), linking a mailbox, giving and taking "read", mailboxes
  without a key, and revocation;
- what phase 3 does **not** protect (section 15).

**Phase 3 hides nothing from the server.** The index stays in clear in the server's database, and
bodies and attachments are fetched live from the provider by the server, which holds the
mailbox's credentials. Phase 3 builds the keys that phase 4 seals content to. Section 15 and the
threat model say exactly what that means.

What each construction is, in the kit:

| Here | Construction | Kit |
|---|---|---|
| Password derivation | Argon2id and the auth/wrap HKDF split | §6.1 to §6.3, with the platform's preparation (§11.2) and bounds (§11.3) |
| Password and recovery wraps | the wrap envelope, one-byte header | §6.5 |
| Recovery code | 150 bits, Crockford's alphabet; the platform's canonical form | §6.7, §11.6 |
| Wrap under the product key | the platform wrap under the kit's Mailie profile | §6.8, Appendix D |
| Browser vault | a key at rest in the browser | §8 |
| Grants | the sealed envelope in direct mode, kind `0x07`, at `GrantRow` | §4, §5 |
| Additional data | the restricted JSON AAD; the browser vault's is the kit's JSON AAD | §2, §8, §10, §11.1 |
| Public keys the server accepts | the platform's check of a product public key | §11.4 |

## 2. Conventions

Kit §2 applies: `||` is concatenation, `u16be` a big-endian 16-bit integer, HKDF is HKDF-SHA256,
AES-GCM is AES-256-GCM with a 12-byte nonce and a 16-byte tag. In addition:

- **A seal id and a namespace** have one spelling: a UUID of version 4 and the RFC 9562 variant,
  as 36 characters of lowercase hyphenated text (`b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4`). Bound
  into bytes, a UUID is its 16 bytes. Upper case, braces, a `urn:` prefix, another version and the
  nil UUID are refused, never normalised (`account-go.json#mailie/account-wrap-aad/refuses/seal-id-in-upper-case`,
  `account-go.json#mailie/account-wrap-aad/refuses/seal-id-in-braces`,
  `account-go.json#mailie/account-wrap-aad/refuses/seal-id-with-a-urn-prefix`,
  `account-go.json#mailie/account-wrap-aad/refuses/seal-id-a-uuidv7`,
  `grant-go.json#mailie/grant-row/refuses/namespace-the-nil-uuid`).
- **An address** is compared, looked up and derived from only as the server stores it,
  `normalise(a)`: white space trimmed at both ends as Go's `strings.TrimSpace` trims it (Unicode
  white space, U+0085 included and U+FEFF not), then every code point lowered by Unicode's simple
  lowercase mapping, as Go's `strings.ToLower` lowers it (`Ä` is `ä`, a Kelvin sign is `k`, `İ` is
  `i`, a capital sigma is `σ` wherever it stands). For every address the server accepts, it is
  exactly what `users.email` holds, the output of `auth.NormalizeEmail`
  (`TestTheSaltIsOfTheAddressAsTheServerStoresIt`). Go `keyscheme.NormaliseAddress`; TypeScript
  `normaliseAddress`, which uses neither ECMAScript's `trim` nor a whole string's `toLowerCase`,
  since both differ (`account-go.json#mailie/normalise-address/next-line-is-white-space`,
  `account-go.json#mailie/normalise-address/zero-width-no-break-space-is-not`,
  `account-go.json#mailie/normalise-address/capital-sigmas-are-each-sigma`,
  `account-go.json#mailie/normalise-address/a-capital-i-with-a-dot-is-an-i`). It validates
  nothing.
- **Text encoding.** Keys, auth keys, proofs, wraps and grants travel as base64url without padding
  (kit §11.1, strict). The one exception is the browser vault's additional data, whose public key
  is standard base64 with padding, because the kit's builder writes it so (kit §8).
- **JSON AAD** is the restricted JSON AAD of kit §11.1: the JCS text of an array of strings from
  `[A-Za-z0-9._:/|@-]` and integers from 0 to 2^31, on which JCS, `JSON.stringify` and Go's
  `encoding/json` agree byte for byte. The one exception is the browser vault's additional data
  (section 7), the kit's JSON AAD of its §2 and §8: its public key is standard base64, whose `+`,
  `/` and `=` are outside that alphabet, so the restricted builder (`platform.JCSArray`) would
  refuse it.
- **Refusals** are named by code (section 13). A refusal is decided before anything is derived,
  sealed or opened whenever the input's spelling alone decides it.

## 3. Identities

### 3.1 The seal id

Every person has a **seal id**, `users.seal_id`: a random UUIDv4 the server draws when the person
is created (and, for the people who exist when phase 3 is deployed, in the migration that adds the
column). For a person who signs up by invitation it is drawn once, when their browser first opens
the invitation, and kept with it, so that the wraps the browser sends with the sign-up are bound
to the seal id the person gets (section 12.1). It is unique and never changes; the schema refuses
an update. It is not secret: the server hands it to the person's browser with their other key
material.

It is what every wrap and grant binds the person by. Not the address: an address changes (id.
reports a new verified one, an operator corrects one) and a binding to it would need a re-wrap at a
moment the person's secret is not in a browser (section 5.5). Not `users.id`: it is `usr_` and 16
hex characters, not a UUID, and the kit's rows and the platform wrap's user id are UUIDs (kit §4.7,
§6.8). Not id.'s `sub`: a self-hosted server has none, and one rule serves both editions.

### 3.2 The namespace

Every mailbox key pair has a **namespace**: a random UUIDv4 the browser that makes the key pair
draws with it (section 8), stored once with the mailbox and never changed. It is the kit's
"tenant" (kit §4.7): the immutable value grants and, in phase 4, sealed content bind to, whatever
workspace the mailbox later belongs to. It is both the tenant and the device of a grant's row
(section 9.1). If the server rewrote it, grants would stop opening: a denial of service, not access.

### 3.3 Epochs

A mailbox's key pairs are numbered by **epoch**: its first key pair is epoch 1, and each new one
(section 12.12) is the previous epoch plus one, up to 65535, the envelope header's `u16be`. Epoch 0
is never used (`grant-go.json#mailie/grant-row/refuses/epoch-0`). The epoch is in every grant's
header, row and HPKE info, so a grant of one epoch never opens as another's
(`grant-go.json#mailie/grant-open/refuses/the-epoch-2-grant-at-epoch-1`), and the server accepts a
grant only at the mailbox's current epoch (`grant-go.json#mailie/grant-shape/refuses/not-the-current-epoch`).

## 4. The account key

A person's **account key** is an X25519 key pair (kit §6.4) made in their browser at enrolment
(section 12): 32 random bytes as the private key, `X25519(sk, 9)` as the public key. Grants are
sealed to its public half; the private half leaves the browser only wrapped (sections 5, 6) and is
kept in the browser only under the vault (section 7).

The server stores the public half once, as `users.public_key`, at the person's own enrolment, and
checks it as the platform checks a product public key (kit §11.4; `keyscheme.CheckPublicKey`):
32 bytes, the canonical encoding of the point, and not of low order, so that the key the server
hands to others has one spelling and a secret can be agreed with it
(`grant-go.json#mailie/public-key-check/*`, Go only). The schema refuses to change it, which stops
the server's own code from changing it by mistake and nothing more: whoever can write the database
can replace it (threat model, section 5.3). The only replacement the server makes is the reset
ceremony (section 12.6), which deletes every grant sealed to the old key in the same transaction.
Every browser that receives the person's wraps compares the key it opens with this public key
(sections 5.5, 6.1, 7).

## 5. The self-hosted password

A self-hosted server never receives the password. The browser stretches it with Argon2id and
splits the result in two: the **auth key**, sent and stored only as a hash, proves who the person
is; the **wrap key**, which never leaves the browser, wraps the account key. This is the kit's
account scheme (kit §6) under Mailie's account profile, `keyscheme.Account()` and `mailieAccount`.

### 5.1 Preparation

A password is prepared by the platform's password profile, `thehappie-password/v1` (kit §11.2):
well-formed Unicode, no run of more than 30 marks, NFC, the space separators turned into U+0020,
no control character, at most 256 code points, then UTF-8. So one password typed two ways derives
one key (`account-go.json#account/derive/nfc` and `account-go.json#account/derive/nfd-with-no-break-spaces`
have the same outputs), and Go and every browser prepare it to the same bytes.

- A password being **presented** (sign-in, a re-derivation, step-up, and the upgrade of section
  12.7 while it existed) has no minimum length. A refused one is `password`/`rejected`
  (`account-go.json#account/derive/refuses/control-character`, `account-go.json#account/derive/refuses/257-code-points`).
- A **new** password (sign-up, a change, a recovery, a reset) has at least 12 code points, checked
  by the console before anything is derived (`prepareNewPassword`; Go
  `platform.PrepareNewPassword`). The server-side rule before this scheme was 10 characters; the
  upgrade kept a shorter existing password, as a presented one, in the one release it existed.

### 5.2 KDF parameters and bounds

```json
{"alg": "argon2id", "m": 65536, "t": 3, "p": 1}
```

`m` is in KiB: 64 MiB, three passes, one lane, a 32-byte output, Argon2id version `0x13`. These are
the server's default parameters, which every account derives with (its target, section 5.3), and
the floor of the bounds, which are the platform's (kit §11.3) and are compiled into both clients:

| | floor | ceiling |
|---|---|---|
| m (KiB) | 65536 | 262144 |
| t | 3 | 10 |
| p | 1 | 4 |
| m × t | | 1048576 |
| salt | exactly 16 bytes | |

A browser refuses parameters or a salt outside these before deriving anything, so a server cannot
make a person's next sign-in a cheap offline target: `kdf`/`out_of_bounds`
(`account-go.json#account/derive/refuses/m-32768`, `account-go.json#account/derive/refuses/m-times-t-over-the-cap`,
`account-go.json#account/derive/refuses/salt-15`) and `kdf`/`unsupported_alg`
(`account-go.json#account/derive/refuses/argon2i`). The console also refuses a member other than
`alg`, `m`, `t` and `p` and a parameter that is not an integer (`checkKDF`), which the kit's
numeric bounds alone would let through. A later release may raise the default within the bounds;
every account then moves to it at its next sign-in (section 5.3).

### 5.3 The salt

The salt is the server's. For an address `a` it is

```
salt(a) = first 16 bytes of HMAC-SHA256(K_salt, UTF-8("mailie/v1/kdf-salt|" || normalise(a)))
```

where `normalise` is section 2's, so the salt is that of the address as `users.email` stores it,
however it is typed, a capital the database's `NOCASE` would not fold included
(`account-go.json#mailie/decoy-salt/non-ascii/a-latin-capital`,
`account-go.json#mailie/decoy-salt/kelvin/a-kelvin-sign`), and `K_salt` is 32 random bytes the
server draws once and keeps with its other secrets (`keyscheme.DecoySalt`;
`account-go.json#mailie/decoy-salt/*`, Go only, since nobody else holds `K_salt`). The challenge
and the sign-in look an account up by `normalise(email)` too.

- **An account's target** is `salt` of its current address with the server's current default
  parameters (section 5.2). Every ceremony that stores a verifier stores the target and nothing
  else: enrolment (sections 12.1 and 12.6, and 12.7 while it existed), a password change and a
  recovery (12.3, 12.4), and
  the re-derivation of a sign-in (12.2, step 5), each in a browser that holds the password. So an
  account leaves its target only when the target moves (its address changes, or the default is
  raised) and returns to it at the person's next sign-in, and a server never moves an account to
  parameters other than its default.
- A **challenge** answers an enrolled account's stored salt and parameters, and, for any other
  address (unknown, disabled, a person with no password, a person from before this scheme who never
  enrolled), `salt(a)` and the default parameters, and nothing more. An account at its target is
  answered exactly what its address would be answered without the account, so a challenge does not
  say whether an address has an account, nor when one was made: the salt an address gets before its
  account exists is the salt it keeps. One window is the exception, and it ends: an account not yet
  at its target, until the person's next sign-in (threat model, section 5.9). The release that
  brought this scheme had a second, the `upgrade` answer (section 12.7), which said an account had
  not enrolled; it left in the next release (threat model, section 5.5).
- A server can still give two addresses one salt; the browser cannot tell (kit §13).

### 5.4 Derivation

```
master   = Argon2id(prepared password, salt (16), m, t, p, dkLen 32, version 0x13)
K_auth   = HKDF(IKM = master, salt = empty, info = "mailie/v1/password/auth", L = 32)
K_wrap   = HKDF(IKM = master, salt = empty, info = "mailie/v1/password/wrap", L = 32)
auth_key = base64url(K_auth)                                                    43 characters
```

This is kit §6.3 with Mailie's labels. `auth_key` is sent; `K_wrap` never leaves the browser, and
in TypeScript is a non-extractable `CryptoKey`. Example: `correct horse battery staple` under the
salt `1TKkuxhQN7cidGB08hZY0Q==` and the default parameters gives the auth key
`aUAj-G3PlQKQsYsCecTgB9OJy1xEDXNi0WPTMHxG9nE` (`account-go.json#account/derive/ascii`).

### 5.5 The account wrap

The account key is kept under the password and under the recovery code in one envelope, the kit's
wrap envelope (kit §6.5) with a one-byte header and no legacy form:

```
aad  = JSON AAD ["mailie/account-wrap", 1, kind, seal_id, base64url(account public key)]
wrap = 0x02 || nonce (12) || AES-256-GCM(K, nonce, account key (32), aad)            61 bytes
```

`kind` is `password` (`K` is `K_wrap`) or `recovery` (`K` is `K_rwrap`, section 5.6). Example
(`account-go.json#mailie/account-wrap-aad/password`):

```
["mailie/account-wrap",1,"password","b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4","3u_7I9EUTrHJW5mYFpPGq-AITIzk6w6MVYtEuC3PQRM"]
```

- **Sealing** (`SealAccountWrap`, `sealAccountWrap`) refuses a kind, a seal id or an account key
  outside its spelling (`binding`: `account-go.json#mailie/account-wrap/refuses/account-key-of-31-bytes`),
  computes the public key from the account key, draws a fresh nonce, and opens what it made and
  compares before returning it (the self-test). Replayed byte for byte with the recorded nonce in
  both languages: `account-go.json#mailie/account-wrap/password`,
  `account-go.json#mailie/account-wrap/recovery`, and an account key whose first byte is zero,
  which some engines mishandle (kit §13): `account-go.json#mailie/account-wrap/account-key-with-a-zero-first-byte`.
- **Opening** (`OpenAccountWrap`, `openAccountWrap`), in order: the binding (`binding`); a wrap
  shorter than 61 bytes is `wrap`/`truncated`; one longer, or not starting with `0x02`, is
  `wrap`/`wrong_key`; the tag; and the opened key's public half must be the bound public key,
  compared in constant time, the key cleared otherwise. Every failure after the binding but a short
  wrap is `wrap`/`wrong_key`: another seal id, another public key, another kind, another wrap key,
  a changed byte, and a wrap that authenticates under its own binding around another account's key
  (`account-go.json#mailie/account-unwrap/refuses/another-seal-id`,
  `account-go.json#mailie/account-unwrap/refuses/another-account-public-key`,
  `account-go.json#mailie/account-unwrap/refuses/the-password-wrap-opened-as-the-recovery-wrap`,
  `account-go.json#mailie/account-unwrap/refuses/another-wrap-key`,
  `account-go.json#mailie/account-unwrap/refuses/flipped-tag`,
  `account-go.json#mailie/account-unwrap/refuses/opens-to-another-accounts-key`). A password wrap
  opens with the password itself, end to end (`account-go.json#mailie/account-unwrap/password/with-the-password`).
- **The server** checks only the shape (`CheckAccountWrapShape`): 61 bytes starting with `0x02`
  (`account-go.json#mailie/account-wrap-shape/refuses/header-0x03`).

**What the additional data binds, and why it differs from Wappie's.** Wappie's wrap AAD is
`"whatserver2/usk|" + email.trim().toLowerCase()` (kit §6.6). Mailie binds:

- **the seal id, not the address.** Wappie re-wraps when an address changes, which it can do
  because its address changes happen in a signed-in browser that holds the password. Mailie's do
  not: id. reports a person's new verified address at sign-in, and an operator corrects one from
  the command line, with nobody's password at hand. A wrap bound to the address would stop
  opening. The seal id never changes, so an address change needs no re-wrap. It also keeps
  Unicode case mapping, which Go and ECMAScript do differently (kit §6.6), out of the AAD.
- **the kind.** Wappie's password and recovery wraps share one AAD; they are kept apart only by
  their keys. Here a wrap moved to the other column also fails at its additional data
  (`account-go.json#mailie/account-unwrap/refuses/the-recovery-wrap-in-the-password-column`).
- **the account public key**, as the platform wrap does (kit §6.8): a wrap opens only for the key
  the server holds for the person and grants are sealed to, and the opener checks it again on the
  opened key.

The header `0x02` is Wappie's too; it is what kit §6.8 requires of a product's other envelopes of
its account key (not `0x03`, the platform wrap's), and the first byte of a grant is `M`. The three
envelopes that hold a person's keys are told apart by their first byte
(`account-go.json#mailie/account-unwrap/refuses/header-0x03-the-platform-wraps`,
`platform-wrap-go.json#mailie/platform-wrap/open/refuses/header-0x02-an-account-wrap`).

### 5.6 The recovery code

Every account has a recovery code from its enrolment; enrolment without one is refused. It is
shown once and replaced at every recovery.

- **Generation** (kit §6.7, §11.6): 30 random bytes, each taken modulo 32 into
  `0123456789ABCDEFGHJKMNPQRSTVWXYZ`, 150 bits, shown as six groups of five joined by `-`
  (`account-go.json#mailie/recovery-code/counting`: bytes 0 to 29 give `01234-56789-ABCDE-FGHJK-MNPQR-STVWX`).
- **Reading it back** is the platform's canonical form C (kit §11.6), not the kit's standard
  normalisation: ASCII spaces, tabs, line breaks and `-` removed, ASCII letters upper-cased, `O`
  read as `0` and `I` and `L` as `1`, and anything else, a `U` or any non-ASCII character
  included, refused, as is a result that is not 30 characters: `recovery_code`
  (`account-go.json#account/normalise/typed-back`, `account-go.json#account/normalise/refuses/a-u`,
  `account-go.json#account/normalise/refuses/a-dotless-i`, `account-go.json#account/normalise/refuses/a-no-break-space`).
  One grammar of recovery codes across the platform and Mailie, read byte by byte, with no
  Unicode case mapping that two languages could do differently.
- **Keys:**

  ```
  K_rwrap        = HKDF(IKM = ASCII(C), salt = empty, info = "mailie/v1/recovery/wrap", L = 32)
  R_proof        = HKDF(IKM = ASCII(C), salt = empty, info = "mailie/v1/recovery/auth", L = 32)
  recovery_proof = base64url(R_proof)                                             43 characters
  ```

  No slow KDF: 150 random bits are not guessed offline (`account-go.json#account/recovery-key/display`,
  `account-go.json#account/recovery-proof/display`). `K_rwrap` wraps the account key as the
  `recovery` kind of section 5.5 (`account-go.json#mailie/account-unwrap/recovery/with-the-code-typed-back`).
  `recovery_proof` is sent; the server releases the recovery wrap only against it.

### 5.7 What the server stores and checks

| Column (informative names) | What | Check |
|---|---|---|
| `users.seal_id` | section 3.1 | drawn by the server; unique; never updated |
| `users.public_key` | section 4 | `CheckPublicKey`; written once |
| `users.kdf_salt`, `users.kdf_params` | sections 5.2, 5.3 | the account's target and nothing else: `salt(normalise(users.email))` and the server's current default |
| `users.auth_verifier` | the auth key, hashed | see below |
| `users.password_wrap`, `users.recovery_wrap` | section 5.5 | `CheckAccountWrapShape` |
| `users.recovery_verifier` | the recovery proof, hashed | see below |
| `users.zk_enrolled_at` | when the person enrolled in this scheme | one way: never set back to 0 (sections 12.6 and 12.7) |

- **The verifiers** are Argon2id PHC strings of the auth key's and the proof's text under the
  server's own parameters, those of an API key's secret (`internal/auth`, `keyParams`: 19 MiB,
  t = 1, p = 1, 32 bytes, a random 16-byte salt per hash), read back from the string so they can be
  raised. The server's hash only has to be one-way: what it hashes is already the output of a
  64 MiB Argon2id of the password, and the password wrap stored beside it is an equally good
  offline oracle (kit §11.7), so the server's hash adds no cost to a guess and its job is to stop a
  copy of the database from being replayed as an auth key. It is the mechanism the server already
  uses for every secret it checks, and costs a sign-in 19 MiB instead of today's 64.
- **An unknown address, a disabled person, a person without a password and a person who never
  enrolled** are checked against a dummy verifier, so a miss costs what a wrong auth key costs and
  answers the same.
- **An old password hash** (`users.password_hash`) is what a person from before this scheme who
  never enrolled still has. Nothing checks it since the upgrade left (section 12.7); the column
  stays, as a migration is never edited, it still says the person has a password (`has_password`),
  and the reset (section 12.6) clears it. An enrolled row has none (a CHECK).
- **The password wrap is handed out only after the auth key is verified**, and the recovery wrap
  only after the recovery proof is: someone without either gets a salt, never a wrap to attack
  offline. Only a copy of the database gives one (threat model, "A backup thief").
- **No route answers a wrap to a session alone.** The password wrap is answered only to an auth key
  verified in the same request (`login`, section 12.2; `password/begin`, section 12.3), the
  recovery wrap only to a recovery proof verified in the same request (`recover/open`, section
  12.4). A session token reaches neither (threat model, section 4.5).
- **Never stored:** the password, `master`, `K_wrap`, the recovery code, `K_rwrap`, the account
  private key.

## 6. The hosted service: the wrap under the product key

On the hosted service people sign in only through id. (the platform's protocol `id-v1`, kit §11).
When a sign-in asks for it, id. delivers to the page the person's product key for Mailie, `sk_p`,
sealed to the page alone (kit §11.12), and the server pins its public half at the first sign-in
(kit §11.15; the core's `identity_key_pins`). The account key is kept under a key derived from
`sk_p`: the platform wrap of kit §6.8 under the kit's Mailie profile (kit Appendix D), which
`keyscheme.PlatformWrap()` and `mailiePlatformWrap` are.

### 6.1 Construction

```
K_pw = HKDF(IKM = sk_p (32), salt = "mailie/platform-wrap/v1",
            info = JSON AAD ["mailie/platform-wrap", 1, seal_id, sub, product_key_id], L = 32)
aad  = JSON AAD ["mailie/platform-wrap", 1, seal_id, sub, product_key_id, base64url(account public key)]
wrap = 0x03 || nonce (12) || AES-256-GCM(K_pw, nonce, account key (32), aad)          61 bytes
```

- **The binding** (`PlatformWrapBinding`, `platformWrapBinding`): the kit's `user_id` is the
  person's **seal id**, never the `sub`. A seal id equal to the sub is refused whatever the sub's
  version (`platform-wrap-go.json#mailie/platform-wrap-binding/refuses/seal-id-the-sub`, with a sub
  of version 4, which the platform does not refuse:
  `platform-wrap-go.json#mailie/platform-wrap-binding/a-uuidv4-sub`), and a wrap bound to the seal
  id does not open with the sub as its user id
  (`platform-wrap-go.json#mailie/platform-wrap/open/refuses/the-sub-as-the-user-id`); `sub` is id.'s
  account id; `product_key_id` is `mailie:<epoch>`, the one the server pinned for the person; the
  public key is `users.public_key`. Example (`platform-wrap-go.json#mailie/platform-wrap/aad/epoch-1`):

  ```
  ["mailie/platform-wrap",1,"b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4","019a8b2c-3d4e-7f60-8a71-b2c3d4e5f607","mailie:1","3u_7I9EUTrHJW5mYFpPGq-AITIzk6w6MVYtEuC3PQRM"]
  ```

- **A departure from kit §6.8.** The kit makes `user_id` the `sub` itself for an account created
  through id., and the product's old id for one it had before. Mailie binds the seal id for every
  person, those created through id. included: one rule for both editions (section 3.1), and a
  value the server draws once rather than one id. issues. The construction takes any lowercase
  UUID as the user id (the kit's own
  `mailie/golden/platform-wrap-go.json#mailie/platform-wrap/seal/user-id-is-not-the-sub`), so no
  byte of the kit moves. But whatever opens a Mailie platform wrap outside the console, such as the
  command-line tool that kit §6.8 and platform decision 0015 have open a product's export from the
  key bundle, must take the user id from the export and never assume the sub: **Mailie's export
  carries each person's seal id beside their platform wrap.** The kit's Appendix D records the
  rule since v0.7.0 (section 17).
- **Sealing and opening** are the kit's, unchanged: sealing refuses an account key whose public
  half is not the binding's and self-tests; opening checks 61 bytes and `0x03`, the tag, and that
  the opened key's public half is the binding's. Every refusal is `platform_wrap`. Products stay
  apart: the same person's Wappie wrap, a Wappie wrap under the very same key bytes, and a Wappie
  wrap with its own binding never open as Mailie's
  (`platform-wrap-go.json#mailie/platform-wrap/open/refuses/the-same-persons-wappie-wrap`,
  `platform-wrap-go.json#mailie/platform-wrap/open/refuses/a-wappie-wrap-under-the-same-key-bytes`,
  `platform-wrap-go.json#mailie/platform-wrap/open/refuses/a-wappie-wrap-with-its-own-binding`).
  The kit's own vectors of the construction (its `mailie/golden/platform-wrap-go.json`, 60 cases,
  36 must fail) are not repeated here: the Go tests run them through `keyscheme.PlatformWrap()`
  (`TestThePlatformWrapRunsTheKitsMailieVectors`), and the kit's TypeScript suite runs them
  against the same package the console uses.
- **Why a symmetric wrap and not a seal to `pk_p`:** anyone holding `pk_p`, the server included,
  could seal an account key of their choosing to it; only a holder of `sk_p` makes this wrap
  (kit §6.8). Nor does id. ever see the account key: it derives `sk_p`, and the wrap is kept by
  Mailie. Opening a person's mailboxes needs id.'s secret **and** Mailie's database.

### 6.2 When the product key is asked for

The product key is asked for (the `account_key` scope, kit §11.14) only when this browser's vault
(section 7) does not hold the person's account key:

- **First sign-in** (the person has no `users.public_key`): the page asks for the key, makes the
  account key, wraps it under `K_pw` and sends the public key and the wrap (section 12.10). Such a
  person is never given a session before that: a sign-in for the identity alone is refused
  (`needs_key`), and the page signs in again asking for the key.
- **A browser without the key** (a new device, or after signing out, which wipes the vault): the
  page asks for the key, the server answers the wrap for the person and the pinned
  `product_key_id`, the page opens it and keeps the account key in the vault. A person who has an
  account key and no wrap at that id (one a reset invitation made, section 17.2) is refused
  (`no_wrap`), with no session.
- **Otherwise** the sign-in is identity only, without the scope (silent when id.'s session allows),
  and the vault is opened after the server names the person (section 7). A silent sign-in proves
  nothing new, so it is no step-up: the session's step-up time is id.'s `auth_time`, never the
  moment of the sign-in (section 11).

A delivered `sk_p` is kept only once the server has named the same `sub`, `product_key_id` and
public key (kit §11.14 step 9), only in memory, and is zeroed once the wrap is sealed or opened.
Recovering the account at id. keeps the root, so `sk_p` and the wrap still open; losing both id.'s
password and its recovery code loses the root, and with it this wrap (section 12.6).

### 6.3 What the server stores and checks

`platform_wraps` (migration 0015): the wrap per person and `product_key_id`, in a column of its own,
since the shape of a wrap is the same for every product (kit §6.8). The server never opens one.

- **Checked:** the wrap's shape with the kit's `platformwrap.CheckShape`
  (`keyscheme.CheckPlatformWrapShape`: 61 bytes starting with `0x03`); the `product_key_id` as
  Mailie's in its one spelling (`keyscheme.ValidProductKeyID`: `mailie:` and an epoch from 1 to
  2^31 − 1 without a leading zero, exactly as id. names it), and pinned for the identity that signs
  in (`identity_key_pins`); the public key as section 4's. The schema holds the wrap's length and
  first byte, and the id's spelling.
- **Insert only:** a trigger refuses an update and a second insert under the same person and id,
  whatever its conflict clause. A wrap is deleted only with its person, or by the reset invitation
  (section 12.6), which replaces the account key and deletes every wrap of the old one in its
  transaction.
- **The first sign-in's ticket** (`external_enrolments`, section 12.10): single use, ten minutes,
  stored as SHA-256, bound to the person, the identity (issuer and `sub`) and the pinned
  `product_key_id`, carrying the sign-in's `auth_time` and session length; deleted with the person,
  by the person's other ceremonies and disabling, by their enrolment, and by the hourly sweep once
  expired.

## 7. The browser vault

The account key is kept in the browser between page loads as the kit's key at rest (kit §8) under
Mailie's profile, `mailieBrowserVault`: a fresh non-extractable AES-256-GCM key, a 12-byte nonce,
a 48-byte ciphertext, and

```
AAD = JSON AAD ["mailie/browser-account-key", 1, seal_id, base64(account public key)]
```

the kit's JSON AAD of its §2 and §8, **not** the restricted one of section 2: the public key is in
standard base64, whose `+`, `/` and `=` are outside the restricted alphabet
(`browser-vault-go.json#mailie/browser-vault/aad/alice` has a `+` and ends in `=`). The record is
kept in IndexedDB beside the session vault (`web/src/state/sessionVault.ts`), one per browser
profile.

- It is written when a sign-in opens the account key, and opened, after the server has named the
  person, only if its seal id and public key are the person's (`openBrowserVault` also recomputes
  the public half, kit §8). A record of anyone else is wiped, never opened; a record that does not
  open is `vault` (section 13), and is wiped too.
- **On a self-hosted server** it is restored with its session at each page load, without asking
  for the password, and wiped at sign-out, in every tab, and whenever a page of the console finds
  no valid session: one that expired, was revoked, or was ended by a password change, a recovery
  or a reset, found when the page loads, when the server refuses its session, or when the
  session's expiry comes while the page is open. Every self-hosted sign-in derives `K_wrap` and
  opens the password wrap anyway (section 12.2), so a record kept past its session would save
  nothing, and would only wait for whoever uses the browser next. Only a running page wipes it:
  a browser closed while signed in keeps the record until the console next runs in that profile,
  whatever happens to the session meanwhile (it expires, or another device signs it out), and a
  copy of the profile taken before then holds the account key (threat model, section 4.6).
- **On the hosted service** it is wiped at sign-out, in every tab, and kept past a session that
  merely expires, so that the same person's next sign-in needs no product key (section 6.2). It is
  kept only where a page of the console established that expiry by the server's clock: its expiry
  timer, which waits by that clock, or a refusal the server's answer dates at or after
  `expires_at` (its `Date`; a refusal that carried none, and a remembered session past its expiry
  by the browser's own clock, are put to the server again). That page marks the record before it
  clears the session's own, and a session that begins with the record clears the mark. The record
  names the session that holds it (its browser login's random id, bound when the session begins),
  and what an ending does reaches only the record its session held, never one a session begun
  since holds, in that page or another tab; a session restored from the browser's own record takes
  only a record that is its own, from before holders, or kept past an expiry, and a ceremony whose
  session ended before it could begin settles the key it kept with it. Everything
  else wipes it, as on a self-hosted server: a refusal before `expires_at` (a revocation, a session
  ended by a password change, a recovery or a reset, a disabled person), a remembered session the
  page gives up on without checking, and no usable session record beside an unmarked key. A page
  that stops halfway leaves either the session's record, which the next page checks again, or an
  unmarked key, which it wipes. A refusal wipes the record only in a page that receives it before
  the session's expiry: a browser closed, asleep or offline until then, or a page that asks
  nothing of the server in between, keeps the record once the expiry has passed, whatever ended
  the session first. That record, and id.'s own session in the same browser, are a trade-off the
  threat model states (section 4.6).
- It is never sent anywhere. Its AES key cannot be exported, so no script reads it out of the
  browser; that does not keep it out of the profile's files. The threat model assumes a copy of
  them carries a usable key, and that a wiped record may stay in them until the browser compacts
  its storage (section 4.6). It does not stop script running in the page (threat model, section
  5.2).

## 8. Mailbox keys

Every mailbox a person links from the console gets a key pair, made by the linker's browser:

- an X25519 key pair, its private key 32 random bytes;
- a namespace, a random UUIDv4 (section 3.2);
- epoch 1.

The browser sends the public key, the namespace and the linker's own grant (section 9) with the
request that links the mailbox, the one request it makes before it leaves for the provider's
consent, with a fresh step-up (section 11), and the server writes them in the transaction that
creates the mailbox, beside the grant of "read" the linker already gets (`GrantLinkTx`). The
mailbox private key never leaves the browser but sealed in grants, and the browser zeroes it once
they are sealed.

| Table (informative names) | What | Check |
|---|---|---|
| `mailbox_keys` | mailbox, epoch, public key, namespace | `CheckPublicKey`; `ValidNamespace`; one row per (mailbox, epoch), written once; the namespace unique and the same for every epoch of a mailbox |
| `mailbox_grants` | mailbox, person, epoch, grant, who gave it, when | section 9.3 |

Mailboxes linked from the command line or by an instance key (the operator workspace's) have no
key in phase 3: there is no person to hold one (section 12.15).

## 9. Grants

### 9.1 Construction

A grant is the kit's grant (kit §5): a direct envelope (kit §4.4) of the mailbox's 32-byte private
key, sealed to one person's account public key, with Mailie's domain (section 10):

```
row    = UUIDv5(namespace, namespace (16) || seal_id (16) || u16be(epoch))     GrantRow(t, d, user, e), t = d = namespace
info   = UTF-8("mlv1/mailbox_grant/" || namespace || "/" || decimal(epoch))
header = 0x4d 0x4c || 0x01 || 0x01 || 0x01 || u16be(epoch) || 0x00                "ML", version, suite, direct, epoch, reserved
aad    = UTF-8("mlv1") || 0x07 || namespace (16) || row (16) || header (8)         45 bytes
grant  = header || enc (32) || HPKE.SealBase(account public key, info, aad, mailbox key)      88 bytes
```

The namespace is both the tenant and the device of the kit's `GrantRow`: a Mailie mailbox is the
kit's "device". Example (`grant-go.json#mailie/grant-aad/alice/epoch-1`): namespace
`9d035f2b-81d0-420e-90e2-bb16e950497b`, seal id `b8cbc8a8-0c90-48ac-9233-fbdace9d7bf4`, epoch 1:

```
info = mlv1/mailbox_grant/9d035f2b-81d0-420e-90e2-bb16e950497b/1
row  = 50332cbe-c6a5-5cc5-97ce-af2ace03df8b
aad  = 6d6c7631 07 9d035f2b81d0420e90e2bb16e950497b 50332cbec6a55cc597ceaf2ace03df8b 4d4c010101000100
```

Binding the seal id stops a grant issued to one person from being presented as another's; the
namespace, from opening another mailbox; the epoch, from opening at another epoch; the kind, from
being taken for a content key (`grant-go.json#mailie/grant-open/refuses/another-user`,
`grant-go.json#mailie/grant-open/refuses/another-namespace`,
`grant-go.json#mailie/grant-open/refuses/another-epoch`,
`grant-go.json#mailie/grant-open/refuses/another-kind-content-key`).

**Sealing** (`SealGrant`, `sealGrant`) refuses a namespace, seal id, epoch or mailbox key outside
its spelling (`binding`), and a recipient key of low order, whose shared secret is all zeros
whatever the private key, so that whoever handed it out could open the grant (kit §4.4):
`invalid_key` (`grant-go.json#mailie/grant-seal/refuses/low-order-recipient/*`, eight encodings).
The seals of the vectors ran under a recorded seed and are replayed on the recorded toolchain; the
TypeScript side opens them and opens its own (`grant-go.json#mailie/grant/alice/epoch-1`).

### 9.2 Opening

`OpenGrant`, `openGrant`, with the person's account private key, the namespace and the epoch, and
the mailbox's public key at that epoch as the server holds it:

1. the binding (`binding`);
2. the kit's header checks: `short`, `magic`, `version`, `suite`, `mode`
   (`grant-go.json#mailie/grant-open/refuses/wappies-magic`,
   `grant-go.json#mailie/grant-open/refuses/a-wappie-device-grant-of-the-same-binding`,
   `grant-go.json#mailie/grant-open/refuses/batch-mode`);
3. HPKE open, refusing an all-zero exchange (kit §4.4);
4. the plaintext is 32 bytes and its public half is the mailbox's public key, compared in constant
   time; the key is cleared otherwise.

Every failure after step 2 is `authentication`, so the opener is no oracle for which binding
failed. Step 4 is Mailie's addition to the kit's grant: **HPKE's base mode does not authenticate
the sender**, and anyone who knows a person's public key, the server included, can seal them a
grant, rightly bound, of a key of their choosing; it opens. Checking the opened key against the
mailbox's public key, written once by the linker's browser, turns that forgery into a refusal
unless the sealer holds the mailbox's real private key
(`grant-go.json#mailie/grant-open/refuses/a-key-the-sealer-chose`). What remains is the threat
model's "unauthenticated grants".

### 9.3 What the server checks, and what it cannot

The server stores a grant only when:

- its shape is a grant's at the mailbox's **current** epoch (`CheckGrantShape`: 88 bytes, `ML`,
  version 1, suite 1, direct mode, that epoch, a zero reserved byte;
  `grant-go.json#mailie/grant-shape/refuses/*`);
- the recipient is an **active member** of the mailbox's workspace with a `users.public_key`,
  written once at their own enrolment, and a seal id, and the grant is one of: "read" being given
  by a person who may give it (an owner or an admin who reads the mailbox now,
  `docs/workspaces.md`); the key supplied to a member who already holds the "read" flag and has no
  grant at the current epoch, by any person who reads the mailbox (section 12.13); or a grant that
  comes with a key its writer writes in the same request, their own or that of a member who holds
  the flag (sections 12.11, 12.12, 12.14);
- the giver reads the mailbox (the flag and a grant at the current epoch) themself, unless the
  grant comes with the key they write; and has a fresh step-up (section 11) when the recipient is
  someone else, and whenever a key is written;
- there is no grant for that (mailbox, person, epoch) yet: a grant is written once, and giving
  "read" again after taking it writes a new one.

It **cannot** check that the 88 bytes seal the mailbox's private key, to the recipient's key, under
the binding: it holds none of those secrets. A grant that is not what it claims fails to open for
its recipient, and nothing else tells: by the rule of section 12.13 it counts its recipient a
reader, so the mailbox is not shown as "waiting for the key" and the key cannot be supplied to
them over it; phase 4, when grants open content, decides the repair (Appendix C). The giver's
browser seals to the public key the server serves for the recipient, refusing one of low order,
and names that key with the grant, which the server refuses unless it is the recipient's now
(Appendix C); it has nothing the server does not serve to check that key against (threat model,
section 5.3).

## 10. The envelope domain and kinds

Mailie's envelope domain (kit §3.1, §4.1, §4.2):

| | Mailie | Wappie |
|---|---|---|
| Magic, bytes 0-1 | `0x4d 0x4c` ("ML") | `0x57 0x53` ("WS") |
| Label (AAD and HPKE info prefix) | `mlv1` | `wsv1` |

- **Magic.** Two ASCII letters naming the product, as Wappie's do. A blob of the other product
  fails at its magic, before anything is derived (`grant-go.json#mailie/grant-open/refuses/a-wappie-device-grant-of-the-same-binding`).
  Its first byte, `M`, differs from `0x02` and `0x03`, the first bytes of the account and platform
  wraps (section 5.5).
- **Label.** `mlv1`: the product and the format's version, as Wappie's `wsv1`. The label opens the
  AAD and the HPKE info, so two products' envelopes never authenticate as each other's even under
  one key and one binding (kit §4.2: two AADs are equal only if their labels are). It is distinct
  from every label of the kit's profiles (kit §3.3; `TestNoLabelOrHeaderIsAnotherProfiles`).

**Kinds.** The byte is bound into the AAD and the name into a direct envelope's HPKE info, so both
are wire format (kit §4.8). The core kinds mean what they mean in every profile:

| Byte | Name | Phase | What |
|---|---|---|---|
| `0x00` | (none) | | never named |
| `0x01` | `headers` | 4, reserved | a message's indexed header fields (subject, addresses, dates), as JSON |
| `0x02` | `snippet` | 4, reserved | a message's preview text |
| `0x03` | `body` | 4, reserved | a message's text, when the index keeps one |
| `0x04` | `attachment_key` | 4, reserved | the 32-byte key of an attachment kept sealed |
| `0x05` | `search_index` | 4, reserved | a segment of the search index the browser builds |
| `0x06` | `content_key` | core | a content key (kit §4.6), at `ContentKeyRow(namespace, namespace, id)` |
| `0x07` | `mailbox_grant` | 3 | a grant (section 9) |
| `0x08` | `user_wrap` | core, reserved | never sealed, as in Wappie |
| `0x09` | `folder_name` | 4, reserved | a folder's name |
| `0x0A` | `draft` | 4, reserved | a draft written in the browser |
| `0x0B`-`0x0F` | (none) | 4, reserved | unnamed, kept for phase 4 |
| `0x10`-`0xFF` | (none) | | unassigned |

An unnamed byte's name is `kind(0x..)`, without leading zeros (`grant-go.json#seal/kind-name/0x0`:
`0x00` is never named). The vectors pin the names of `0x00` and of the named kinds only: a reserved
or unassigned byte may be named by a later version, so its name today is no promise
(`TestTheKindsAreAProfileTheKitAccepts` checks the spelling). Phase 3 seals only `mailbox_grant`.
The phase 4 kinds are named now so that no byte is ever given two meanings; nothing is sealed under
them in phase 3, and phase 4 defines their plaintexts and rows. A name never changes once a value
has been sealed under it in direct mode.

## 11. Step-up

Some actions need the person to have proved who they are within the last **10 minutes**. The
session records when (`sessions.authenticated_at`, its step-up time), and the server refuses those
actions when that time is more than 10 minutes old, or later than the server's own clock.

- **What needs it:**
  - giving "read" to someone else, and supplying the key to someone who holds the flag (posting a
    grant for another person, sections 12.13 and 12.14);
  - writing any row of `mailbox_keys`, with the grants that come with it: linking a mailbox
    (section 12.11), a new key (section 12.12), and the first key of a mailbox that has none
    (section 12.14).

  Changing the password and replacing the recovery code present the current auth key in their own
  request instead (sections 12.3 and 12.5), window or not: each sets a secret of the person's, and
  a recovery code a session could set would be a password it could set (section 12.4).
- **What sets the time, self-hosted:** a ceremony in which the server verified the person's own
  secret and opened the session (signing up, section 12.1; signing in, 12.2; a reset invitation,
  12.6; in the release that brought this scheme, the upgrade's enrolment, 12.7), and a step-up; on
  the server's clock.
- **What sets the time, hosted:** id.'s `auth_time`, from the userinfo of the access token the
  sign-in (section 12.8) or the step-up presented, never the server's clock; a step-up's
  `auth_time` later than the server's now is refused, and a sign-in's is taken as now (Appendix C).
  A first sign-in's enrolment (section 12.10) opens the session with the `auth_time` of the
  sign-in that issued its ticket. A silent sign-in (identity only, answered from id.'s session,
  section 6.2) carries id.'s earlier authentication time, so it opens no window unless that
  authentication was itself within the last 10 minutes.
- **A sign-in counts.** For 10 minutes after a sign-in that sets the time, the session needs no
  further step-up, so that the console can key mailboxes right after a sign-in (section 12.14)
  without asking for the password twice. The cost is stated, not hidden: a session token copied
  within those 10 minutes can do every action above (threat model, sections 4.5 and 5.12), and
  none that sets the person's password or recovery code.
- **A step-up proves the session's own person, and refreshes that one session only.**
  - Self-hosted: `stepup {auth_key}` on the session. It carries no address: the browser derives
    the auth key under the stored salt and parameters of the session's person (which `challenge`
    answers for their address), and the server checks it only against `users.auth_verifier` of
    the session's person, under the sign-in rate limits. A step-up that does not verify changes
    nothing.
  - Hosted: the page starts a step-up on its session, and the server records a mark: its time,
    bound to that session, single use, valid for 10 minutes. The page signs in at id. again with
    `prompt=login` and without the `account_key` scope, and posts the access token to `stepup` on
    the same session. The server reads id.'s userinfo and refuses unless its issuer and `sub` are
    the external identity linked to the session's person, its `client_id` is Mailie's, its
    `auth_time` is after the mark (in whole seconds, strictly: id. and Mailie share one host
    clock in production) and not after the server's now, and the product key it names is the one
    pinned under its `product_key_id` (compared, read only: a step-up never pins, and another key,
    or an id nothing is pinned under, is refused and raises the alert a sign-in raises); then it
    consumes the mark and sets that session's step-up time to `auth_time`. A step-up as anyone
    else is refused and changes nothing, whatever its `auth_time`: id. never compares `login_hint`
    with the account that signs in, so the server compares the `sub`.
  - Neither creates a session, changes another one, or changes whose the session is. The
    platform's own console does it the other way (`id-v1`, "Step-up": a step-up is a fresh sign-in
    that replaces the session, so a session is always its last authenticator's); Mailie keeps the
    session and checks the identity instead.
- **Tested with the server's code** (`internal/auth/accountkeys_test.go`,
  `TestAStepUpProvesOnlyTheSessionsOwnPerson`, `TestAnExternalStepUpNeedsAFreshMarkAndTheSessionsOwnIdentity`,
  `TestAnExternalStepUpComparesTheProductKeyWithThePinAndNeverPins`,
  `TestAnExternalSignInsStepUpTimeIsTheProvidersNeverTheSignIns`; for the actions on mailbox keys,
  `internal/service/mailboxkeys_test.go`, `TestEveryKeyWriteNeedsAStepUpWithinTenMinutes` and
  `TestChangingFlagsWithoutAGrantNeedsNoStepUp` and `TestALinkWhoseStepUpEndedDuringItsLoginStoresNothing`,
  and over REST `TestTheMailboxKeyRoutesAreThinOverTheService` and
  `TestNoRouteTakesAMailboxPrivateKey`): a step-up as another person or
  another id. identity is refused; a silent sign-in does not freshen the time; each action above
  is refused past the 10 minutes, writing nothing, and accepted within them after a sign-in or a
  step-up; and what writes no key nor a grant for someone else (flags alone, "read" by the flag to
  a person without an account key) needs none. That a
  session alone, right after its sign-in, does not replace the recovery code is
  `TestReplacingTheRecoveryCodeNeedsTheCurrentAuthKey` and, over REST,
  `TestASessionAloneCannotReplaceTheRecoveryCodeEvenRightAfterSignIn`.

**Why.** In phase 3 the server decides who reads a mailbox from the "read" flag and the existence
of a grant row at the current epoch, and it cannot tell a real grant from 88 random bytes of the
right shape (section 9.3), nor a mailbox key a person's browser made from one an attacker made
(section 12.14). Without the step-up, a stolen session of an owner could give "read" to an
accomplice with such bytes, and a stolen session of a reader could choose a keyless mailbox's
permanent key. With it, outside the 10 minutes after a sign-in, it takes the person's password (or
a fresh id. sign-in as that person) too. In phase 4 the grant itself is what opens the sealed
content.

## 12. Ceremonies

Each ceremony says what the browser does, what it sends, and what the server checks and stores in
one transaction. "Shape" is the server's check of section 5.7, 6.3 or 9.3.

### 12.1 Signing up by invitation (self-hosted)

1. `signup/open {invite, email}` → `{salt, kdf, seal_id}`: `salt(email)` and the default
   parameters, the address's target (section 5.3), which is what a challenge answers an address
   without an account; and the seal id of the person the invitation will create, drawn by the
   server the first time it is opened and kept with the invitation (section 3.1). The server
   checks the invitation as the sign-up will, and changes nothing else.
2. The browser prepares the new password (at least 12 code points), derives `auth_key` and
   `K_wrap`, makes the account key and a recovery code, and seals the password and recovery wraps
   under that seal id.
3. `signup {invite, email, seal_id, auth_key, kdf, public_key, password_wrap, recovery_wrap,
   recovery_proof}`. The server checks the invitation as today (it names `normalise(email)`), that
   `seal_id` is the one step 1 answered for it, that `kdf` is its current default, the public key
   and the wraps' shapes, and stores section 5.7's columns with the account's target
   (`salt(normalise(email))`, the salt the browser derived with, since `salt` normalises), that
   seal id and `zk_enrolled_at` set. The answer carries the session, whose step-up time is now
   (section 11), the seal id and the public key.
4. The browser checks that the answer names the public key it made (an answer that names another
   is a security error, and its session is ended), keeps the account key in the vault, and shows
   the recovery code once.

### 12.2 Signing in (self-hosted)

1. `challenge {email}` → `{salt, kdf}`, for `normalise(email)`, and nothing more (section 5.3).
2. The browser checks `kdf` and the salt (section 5.2), derives, and sends `login {email, auth_key}`.
3. The server looks the person up by `normalise(email)` and verifies the auth key (a dummy verifier
   for every other case). It answers the session, whose step-up time is now (section 11),
   `seal_id`, `public_key` and `password_wrap`; and, when the account's stored salt and parameters
   are not its target (section 5.3), the target `{salt, kdf}` and a re-derivation ticket (single
   use, 10 minutes, bound to the person, that session and the auth key just verified, of which it
   keeps SHA-256 of the 32 bytes; stored as SHA-256).
4. The browser opens the wrap with `K_wrap` under `seal_id` and `public_key` (section 5.5): a wrap
   that does not open is a security error, not a wrong password, since the auth key was accepted.
   It keeps the account key in the vault. Whatever the challenge answered, it sends nothing but
   the auth key: no password goes to the server in any ceremony.
5. **Re-derivation.** If the answer named a target, the browser prepares the same password again,
   as presented, checks the target's parameters and salt (section 5.2), derives under them, wraps
   the account key under the new `K_wrap`, and sends `password/finish {ticket, current_auth_key,
   auth_key, kdf, password_wrap}` (section 12.3), where `current_auth_key` is the auth key of
   step 2, sent again. The server refuses, before any hash and leaving the ticket unused, a
   `current_auth_key` whose SHA-256 is not the one kept with the ticket: the ticket rides in the
   same answer as the session, and whoever saw only that answer (a proxy's log) must not set the
   password with it (threat model, sections 4.5 and 5.12). It checks that `kdf` is its current
   default, and stores the target, the new verifier and the new wrap. The password, the account
   key, the grants and the recovery wrap do not change, and no session ends. If the step does not
   happen (the tab closes), the next sign-in asks again.

### 12.3 Changing the password

1. The browser asks `challenge {email}` for the account's stored salt and parameters, prepares the
   current password as presented, and derives the current auth key and `K_wrap`.
2. `password/begin {current_auth_key}` on the session → `{password_wrap, salt, kdf, ticket}`: the
   current wrap and the account's target, answered only to a current auth key verified in this
   request, under the sign-in rate limits; the ticket is single use, 10 minutes, bound to the
   person, the session and that current auth key (SHA-256 of its 32 bytes, kept with the ticket),
   stored as SHA-256. A session alone gets nothing (section 5.7).
3. The browser opens the wrap with the current `K_wrap` (and, when the vault holds the account
   key, compares the two), prepares the new password as new (section 5.1), checks the target's
   parameters and salt, derives under them, and wraps the same account key under the new
   `K_wrap`.
4. `password/finish {ticket, current_auth_key, auth_key, kdf, password_wrap}`, with the
   `current_auth_key` of step 2. The server refuses, before any hash and leaving the ticket
   unused, a `current_auth_key` the ticket was not issued to: a session and the answer of step 2,
   copied, set nothing. It checks that `kdf` is its current default, and stores the target, the
   new verifier and the new wrap; the account key, its public key, the grants and the recovery
   wrap do not change. Other sessions end, as today, unless the ticket is a sign-in's
   re-derivation (section 12.2, step 5).

### 12.4 Recovery

1. `recover/open {email, recovery_proof}` → `{seal_id, public_key, recovery_wrap, salt, kdf,
   ticket}`, for `normalise(email)` and only against the stored proof (a dummy for every other
   case); `salt` and `kdf` are the account's target (section 5.3); `ticket` is single use,
   10 minutes, bound to the person and to the recovery proof just verified, of which it keeps
   SHA-256 of the 32 bytes, and stored as SHA-256.
2. The browser opens the recovery wrap with `K_rwrap` (section 5.6), prepares a new password as
   new, checks the target's parameters and salt, derives under them, wraps the same account key
   under the new `K_wrap`, makes a **new** recovery code and wraps the account key under it.
3. `recover/finish {ticket, current_recovery_proof, auth_key, kdf, password_wrap, recovery_wrap,
   recovery_proof}`, where `current_recovery_proof` is the proof of step 1, sent again, and
   `recovery_proof` the new code's. The server refuses, before any hash and leaving the ticket
   unused, a `current_recovery_proof` whose SHA-256 is not the one kept with the ticket: the
   ticket rides in the same answer as the recovery wrap, and whoever saw only that answer (a
   proxy's log) must not set a password and a code of their own with it (threat model, section
   5.12). It checks that `kdf` is its current default and stores the target with the rest. The
   account key is unchanged, so every grant still opens. Every session ends, and the person signs
   in with the new password (section 12.2). The browser shows the new code once. A sign-in after
   it that fails (the account's rate limit, the network) leaves the recovery done: the browser
   says so and asks for the new password, never for another recovery, which would only replace
   the code it just showed.

### 12.5 Replacing the recovery code

1. The browser asks `challenge {email}` for the account's stored salt and parameters, prepares the
   current password as presented, and derives the current auth key, as a password change's first
   step does (section 12.3); it makes a new code and wraps the account key its vault holds under
   it.
2. `recovery {current_auth_key, recovery_wrap, recovery_proof}` on the session. The server checks
   the current auth key against the session's person's verifier only, under the sign-in rate
   limits (a session alone, whatever its step-up time, sets nothing), and, in the transaction that
   stores the new wrap and proof, that the verifier is still the one it checked. It deletes the
   person's recoveries opened with the old code (section 12.4), and nothing else: the password is
   unchanged. The browser shows the new code once.

### 12.6 The reset invitation, and a lost key

`mailserver user password --bootstrap` becomes a **reset invitation**: a single-use link for the
person, like an invitation (7 days, the code in the fragment, stored as SHA-256). Nothing remote
sets someone's password, as today. Opening it is section 12.1 with the reset code instead of an
invitation, step 4 included, but for its step 1: the browser asks `reset/open {reset, email}` →
`{salt, kdf, seal_id}`, the account's target (section 5.3) and the person's seal id, which does
not change. Not a challenge, which answers an account off its target the salt it stores now: the
reset stores the target, so a password derived under the challenge's answer would never sign in
again. It gives the person a **new** password, recovery code and account key. In one transaction
the server replaces `users.public_key` (the one replacement of a written-once key), the verifiers
and wraps, and **deletes every grant sealed to the old key** and
every platform wrap of the person, and ends their sessions. Their personal mailboxes then open
only once they write them a new key (section 12.12); a team's mailboxes, on which they keep the
flag, once a reader supplies them the key (section 12.13). The reset is also the way back for a
person from before this scheme who never enrolled (section 12.7): it enrols them, under the seal
id they always had, and clears their old password hash.

**The last reader.** Deleting a person's grants takes "read" from them on every mailbox that has a
key (section 12.13), and a team mailbox left with no reader can never be given a key again
(section 12.12). So the reset holds the core's last-reader rule (`docs/workspaces.md`):
`mailserver user password --bootstrap` refuses, naming the mailboxes, when the person is the last
reader (the flag and a grant at the current epoch) of any team mailbox, unless it is given
`--force`, which the invitation records. Any team mailbox: the test closing a person uses leaves
out a team whose only member is the person, since that team goes with them, but a reset leaves the
person and every team of theirs standing. Opening an invitation issued without force
(`reset/open`) and completing it check again, the second in its transaction, and refuse if the
person has become such a last reader since; the operator then has someone else given "read" first,
or issues it again with force. Force fits a key that is truly lost, whose grants open nothing
anyway; when the reset is a precaution against a copied key (threat model, section 4.6), another
reader comes first.

The hosted service's equivalent, a person whose platform wrap no longer opens because id. issued
them a new root (they lost both id.'s password and its recovery code, which by design loses what
the root opened), does not arise in id. v1, which has no key reset: the root never changes for a
`sub`, and such a person makes a new id. account, a new `sub`, which signing in refuses for an
address that has a person here (an identity never takes over a person). The operator closes the
old person first (`user disable`, then `user delete`), and the new identity signs in as a new
person (section 17, question 2).

### 12.7 The upgrade of existing self-hosted accounts (removed)

People who signed up before phase 3 had a password hashed on the server and no account key. The
release that brought this scheme let each of them send their password to the server **one last
time**, at their first sign-in on it, and enrol; the next release removed it, as section 17 had
settled (Appendix C). This section is history: what the upgrade was, when it left, and what stays.

**What it was.** Their challenge answered `{salt: salt(email), kdf: default, upgrade: true}`, only
for an active person with a server-side password hash and `zk_enrolled_at = 0`. Their browser,
unless it remembered the address as enrolled (a memory of every address that enrolled or proved a
zero-knowledge secret in it, keyed by the server's origin and `normalise(address)`, kept in
IndexedDB and not wiped at sign-out), sent `upgrade/login {email, password}` over TLS. The server
checked the password against the old hash, under the old rules and the sign-in limits, and
answered a single-use enrolment ticket (10 minutes, bound to the person and to no secret, stored as
SHA-256), the person's seal id and the address's target, never a session. The browser prepared the
same password as presented (or a new one, when the profile refused it), derived under that target,
made the account key and a recovery code, sealed the wraps under that seal id, and sent
`upgrade/enrol {ticket, auth_key, kdf, public_key, password_wrap, recovery_wrap, recovery_proof}`.
In one transaction the server stored section 5.7's columns, set `zk_enrolled_at` (one way), cleared
the old hash, ended the person's other sessions and opened one whose step-up time was now.

**When it left.** In the release after the one that brought this scheme (2026-10-10). The routes
`/v1/auth/upgrade/login` and `/v1/auth/upgrade/enrol` are not found, the challenge answers
`{salt, kdf}` and nothing more, and the server checks no password in clear. The console sends no
password at all, whatever a challenge answers (an `upgrade` member is ignored, as any member it
does not know), so the memory of enrolled addresses, which defended only the upgrade, is gone too:
the browser vault's database (section 7) drops its store, and the addresses in it, at its next
open.

**What stays.** A person who never signed in during that release has not enrolled. Their old hash
stays in `users.password_hash`, which no sign-in checks (section 5.7); the schema is unchanged, with
no migration: the column, the one-way trigger on `zk_enrolled_at`, and `auth_tickets`' CHECK, which
still admits the purpose `enrol`, stay as migration 0013 wrote them. Nothing issues an `enrol`
ticket now and no ceremony takes one, so one left from the release before finishes nothing and goes
with its ten minutes. Such a person is answered and refused exactly as an address without an
account: the challenge answers the address's target (section 5.3), and every sign-in fails as a
wrong one does, after one derivation against the dummy verifier (section 5.7). Their way back is
the operator's reset invitation (section 12.6, `mailserver user password --bootstrap --email
ADDRESS`), which enrols a person who was not and clears the old hash; the release notes
(`docs/self-hosting.md`) say so. With no password sent anywhere, the residuals the upgrade left
(threat model, sections 5.5 and 5.12) ended with it.

### 12.8 Signing in on the hosted service

As section 6.2. The server's answer to the page names the person's `sub`, `product_key_id` and pinned
key (kit §11.15), and, for Mailie, `seal_id`, `public_key` and, when the page asked for the key, the
platform wrap for that `product_key_id`. The page says whether it asked: id.'s userinfo names the
product key whether or not the authorization did. The page opens the vault or the wrap, and
compares the account key's public half with `public_key` either way. The server sets the new
session's step-up time to the userinfo's `auth_time`, at most its own now, never to the moment of
the sign-in (section 11).

Only a person who has an account key is given a session. One who has none is answered, when the
page asked for the key, the ticket of section 12.10 and no session, and otherwise refused
(`needs_key`), creating and linking nothing. One who has an account key and no wrap at the pinned
`product_key_id` is refused when the page asked for the key (`no_wrap`, section 17.2), with no
session.

### 12.9 Step-up

Section 11.

### 12.10 First sign-in on the hosted service

The page asked for the product key; the person has no public key yet. The server answers, in place
of a session, a single-use enrolment ticket (ten minutes, stored as SHA-256, bound to the person,
the identity and the pinned `product_key_id`, section 6.3) and the person's seal id. The page makes
the account key, seals the platform wrap under the delivered `sk_p` for (seal id, sub, pinned
`product_key_id`, public key), and sends `{ticket, public_key, platform_wrap}` with the
`product_key_id` it sealed under. In one transaction the server uses the ticket (unexpired, issued
for that `product_key_id`, its person active and its identity still theirs), writes `public_key`
once and the wrap insert only, both checked for their shapes, and opens the session, whose length
and step-up time are those of the sign-in that issued the ticket. No session exists before: a
session token copied from a browser never chooses a person's account key. There is no password and
no recovery code on the hosted service: recovery is id.'s (section 6.2).

### 12.11 Linking a mailbox

Section 8. The browser makes the key pair and the namespace, seals the linker's grant at epoch 1
to their own public key, and sends `{public_key, namespace, grant}` with the request that creates
the mailbox (every path that links one: the password form and the web OAuth flow), with a fresh
step-up (section 11). Resuming an abandoned link sends no key: the mailbox it resumes holds the
one its link wrote (Appendix C). The server checks the step-up, the public key,
the namespace's spelling and uniqueness, and the grant's shape at epoch 1 for the linker. It never
makes a mailbox key itself.

### 12.12 A new key for a mailbox

A **personal** mailbox's person may write it a new key pair at the next epoch, with their own grant,
after a fresh step-up: when they can no longer open its grant (after a reset, section 12.6), or at
will. The server writes the new `mailbox_keys` row and grant and **deletes every grant at older
epochs**; the new epoch is the only one it accepts from then on. In phase 4 this means syncing the
mailbox again from the provider.

A **team** mailbox is never given a new key: an owner or an admin who generated one would hold the
only grant and could choose who reads, taking the mailbox from its readers. A team mailbox nobody
can open stays "nobody can read" and can only be removed and linked again, which takes the
provider's consent (proof of control of the mail account) and makes a new mailbox, namespace and
key.

### 12.13 Giving and taking "read"

- **Who reads.** On a mailbox that has a key, "read" requires both the flag and a grant at the
  current epoch, for every person, decided in `internal/service`. On a mailbox that has none
  (section 12.14), the flag alone, today's rule. This one rule decides access, whether a team
  mailbox syncs and has its consent (both need a reader, `docs/workspaces.md`), and the last-reader
  protection.
- **Giving:** an owner or an admin who reads the mailbox now (the rule of `docs/workspaces.md`)
  opens their own grant in the browser, seals the mailbox key to the recipient's public key at the
  current epoch, and sends the flag and the grant together, with a fresh step-up (section 11). The
  server checks section 9.3. To a member who has not enrolled yet, and on a mailbox without a key,
  "read" is given as today, with the flag alone; the key follows as below.
- **Supplying the key.** A member who holds the flag but no grant at the current epoch (they had
  not enrolled when the key was written, or were reset, section 12.6) sees the mailbox as "waiting
  for the key" and does not read it. Any person who reads the mailbox may seal them the key at the
  current epoch, with a fresh step-up; their console lists the members waiting and offers it, and
  the console of the member waiting says who can. The server checks section 9.3, and that the
  recipient holds the flag now. This gives nobody "read" who was not given it; it gives the key to
  someone who was, and so needs no owner or admin, who may read none of the team's mailboxes.
- **Taking:** removing "read", a member leaving the workspace, being disabled or deleted deletes
  the person's grants for the mailbox (all epochs) in the same transaction as the flag. Giving it
  back needs a new grant.
- **The last reader** of a team mailbox is the last active member who reads it by the first
  bullet: holding the flag **and** a grant at the current epoch on a mailbox that has a key, the
  flag on one that has none; keys and roles never count. Every path that takes "read" away keeps
  the protection, the reset of section 12.6 included.

### 12.14 Mailboxes without a key

Mailboxes that exist when phase 3 is deployed have no key, and are read as today, by the flag
(section 12.13).

- **The first key.** A person who reads such a mailbox now and has enrolled writes its key pair and
  namespace at epoch 1, with their own grant and one for every other active member who holds the
  flag and has a `users.public_key`, in one request, with a fresh step-up (section 11). The server
  checks the key and the namespace as at linking (section 12.11), that the mailbox has no key yet,
  and each grant by section 9.3, and writes them in one transaction. Nobody else may write a
  mailbox's first key, so that an owner or an admin who does not read a team mailbox cannot take it
  by keying it.
- **When.** The console writes first keys right after a sign-in, while its step-up time is fresh,
  and otherwise asks for a step-up first; it never writes one on the page load of an older
  session.
- **After.** From the first key on, "read" is the flag and a grant. A member who held the flag but
  had no public key yet (they had not enrolled) sees "waiting for the key" until a reader supplies
  it (section 12.13).

### 12.15 Operator mailboxes and API keys

Mailboxes linked from the command line or by an instance key have no key in phase 3, and API keys
are unchanged: nothing is sealed yet. Phase 4 decides both with service identities.

## 13. Errors

| Code | Reason | Meaning | Go | TypeScript |
|---|---|---|---|---|
| `binding` | | an input outside its spelling (section 2), refused before anything is derived, sealed or opened; a platform wrap's seal id equal to the sub (section 6.1) | `keyscheme.ErrBinding` (the kit's `mailie.ErrBinding`) | `MailieError` `binding` |
| `shape` | | the server's check of a wrap or grant it stores | `keyscheme.ErrShape` (the kit's `mailie.ErrShape`) | `MailieError` `shape` |
| `public_key` | | a public key the server refuses to store (section 4) | `keyscheme.ErrPublicKey` (the kit's `mailie.ErrPublicKey`) | (the server's only) |
| `password` | `rejected` | the preparation refused a password being presented | `account.Error` | `AccountError` |
| `password_too_short`, `password_too_long`, `password_invalid` | | the platform's preparation refused a new password (section 5.1), before anything is derived | `platform.ErrPasswordTooShort`, `ErrPasswordTooLong`, `ErrPasswordInvalid` (`platform.ErrorCode`) | `PlatformError` with that code (`prepareNewPassword`) |
| `kdf` | `out_of_bounds`, `unsupported_alg` | parameters or salt outside section 5.2 | `account.Error` | `AccountError` |
| `wrap` | `truncated`, `wrong_key`, `bad_key` | an account wrap that does not open (section 5.5); `bad_key` is a wrap key that is not 32 bytes, Go only | `account.ErrTruncated`, `ErrWrongKey`, `ErrBadKey` | `AccountError` |
| `recovery_code` | | not a recovery code (section 5.6) | `platform.ErrRecoveryCode` | `PlatformError` `recovery_code` |
| `platform_wrap` | | a platform wrap that cannot be sealed or opened (section 6) | `platformwrap.ErrPlatformWrap` | `PlatformWrapError` |
| `short`, `magic`, `version`, `suite`, `mode` | | a grant's header (section 9.2) | `seal.ErrShort` and the rest | `SealError` |
| `authentication` | | a grant that does not open as this person's grant of this mailbox's key at this epoch | `seal.ErrAuthentication` | `SealError` |
| `invalid_key` | | sealing to a missing or low-order public key | `seal.ErrInvalidKey` | `SealError` |
| `vault` | | a browser vault record that does not open for the person named (section 7): another seal id, another public key, not a record; the console wipes it | | `MailieError` `vault` (`openBrowserVault`, `openBrowserVaultKey`) |

Codes, never messages, cross the boundary; the console translates them (as it does every server
code), and no message repeats a key, a password, a code or an address. The server's answers to the
ceremonies use its seven API codes and never say which credential failed.

## 14. Vectors

**Format.** The kit's (kit §12.1, `thehappieco-kit-vectors/1`): files of cases with an `id`, an
`op`, `in`, and exactly one of `out` and `error` (with `reason` for the account codes); bytes in
standard base64 in `_b64` fields, UUIDs as text; `langs` narrows a case to the languages that run
it. Ops shared with the kit keep its names and members (`account.derive`,
`account.normalise_recovery_code`, `account.recovery_key`, `account.recovery_proof`,
`seal.kind_name`, `mailie.platform_wrap_*`); the others are `mailie.*`.

| File | Module | Cases | Must fail | What |
|---|---|---|---|---|
| `account-go.json` | account | 104 | 60 | profile, an address as the server stores it, server salt (Go only), derivations at the floor, recovery code, account wraps |
| `grant-go.json` | seal | 107 | 69 | domain and kinds (the named ones and `0x00`), the server's check of a public key (Go only), rows, infos, AADs, grants sealed by Go and their refusals, the server's shape check; named keys under `keys` |
| `platform-wrap-go.json` | platformwrap | 31 | 18 | product keys, Mailie's binding, wraps under it, Wappie's wraps refused |
| `browser-vault-go.json` | browser_account | 6 | 3 | the vault's AAD |

**Who writes, who reads.** Go writes every file (`internal/keyscheme/vectors_test.go`):

```sh
go test ./internal/keyscheme -run TestTheVectorsAreWhatTheProfileWrites -update
```

Nothing in them is random: keys, salts, roots and nonces are SHA-256 of fixed labels, and every
seal that draws randomness runs under `testing/cryptotest.SetGlobalRandom` with the seed its case
records. Before a file is written every case is run through the Go dispatcher, so a refusal is
recorded only if the code refuses it. Without `-update`, on the toolchain the files name, Go
builds them again and compares byte for byte; on any toolchain it computes, opens or refuses every
case again (`TestTheGoSideOpensEveryVector`). TypeScript (`web/test/keyscheme.spec.ts`) runs every
case marked for it with `web/src/crypto/mailie.ts`: it computes, replays the account wraps and
platform wraps byte for byte by injecting the recorded nonce, opens Go's grants (and seals and
opens its own), and refuses every refusal with its code.

**Reused, not repeated.** The kit's vectors already pin the constructions Mailie only
parameterises: its `mailie/golden/platform-wrap-go.json` (run by Go through this profile, section
6.1), the low-order forgeries of the sealed envelope and HPKE (its `kit/seal-go.json#seal/direct/forged/grant/*`,
`kit/hpke-go.json#hpke/open/forged/*`, run by the kit against the same packages), the platform's
password preparation and recovery code (its `platform/id-v1/password-profile.json`,
`recovery-code.json`). Mailie's files pin what Mailie chose: its labels, headers, magic, kinds,
AADs, bindings and the refusals they add.

## 15. What phase 3 does not protect, and what phase 4 adds

Phase 3 does **not** keep any mail from the server:

- the **index** (headers, snippets, folder names, the search index) stays in clear in the
  server's SQLite database and in its backups;
- **bodies and attachments** are fetched live from the provider by the server when the console
  asks, and pass through it in clear;
- the server holds every mailbox's **credentials** (OAuth tokens and passwords, under its sealer:
  the local keyring, or AWS KMS on the hosted service) and can fetch any mail at any time;
- **API keys and MCP** read through the server, as before;
- the server decides access as before; a grant row is a second condition it checks, not a
  cryptographic barrier (section 11).

What phase 3 does give: no server stores or receives a self-hosted person's password once they
have enrolled; a copy of the database yields only material that needs the person's password,
recovery code or id. secret to open; every key a person or a mailbox will need in phase 4 exists,
is bound to the right people as far as the public keys the server serves are theirs (threat model,
section 5.3), and is fixed by this document.

**Phase 4** seals the index to each mailbox's public key (batch envelopes under content keys,
kind `0x06` at `ContentKeyRow(namespace, namespace, id)`, and the reserved kinds of section 10),
moves search into the browser, asks a new consent to sync, decides service identities for API keys
and operator mailboxes, and decides whether taking "read" re-keys a mailbox, and whether browsers
pin the public keys they seal to and remember a mailbox's keys across epochs (threat model,
sections 5.3 and 5.4).

## 16. The code

The profile is the kit's (kit Appendix D): Go `github.com/thehappieco/kit/profiles/mailie`,
TypeScript `@thehappieco/kit/profiles/mailie`, both since v0.7.0. The core names it in one place on
each side: Go [`internal/keyscheme/keyscheme.go`](../internal/keyscheme/keyscheme.go) gives the
kit's values the core's names (type aliases, the kit's constants and sentinels, and forwarders),
and TypeScript [`web/src/crypto/mailie.ts`](../web/src/crypto/mailie.ts) re-exports the kit's
module whole. Neither restates what the kit publishes; what the kit leaves to Mailie (the salt the
server hands out, the address as it stores it, drawing seal ids and namespaces) stays beside it:
Go `internal/keyscheme/server.go` and, in `mailie.ts`, the console's helpers.

| What | Go (`internal/keyscheme`) | TypeScript (`web/src/crypto/mailie.ts`) |
|---|---|---|
| Account profile, labels, bounds | `Account`, `DefaultKDF` (the kit's) | `mailieAccount`, `DEFAULT_KDF`, `checkKDF`, `prepareNewPassword` (the kit's) |
| Addresses, server salt | `server.go`: `NormaliseAddress`, `DecoySalt`, `SaltLabel` (the core's) | `normaliseAddress` (the console's) |
| Account wraps | `AccountWrapAAD`, `SealAccountWrap`, `OpenAccountWrap`, `CheckAccountWrapShape` (the kit's) | `accountWrapAAD`, `sealAccountWrap`, `openAccountWrap`, `checkAccountWrapShape` (the kit's) |
| Seal ids, namespaces, public keys | `ValidSealID`, `ValidNamespace`, `PublicKey`, `CheckPublicKey`, `ErrPublicKey` (the kit's); `server.go`: `NewSealID` (the core's) | `isSealID`, `isNamespace` (the kit's); `newNamespace` (the console's) |
| Domain, kinds, grants | `SealDomain`, `Kind`, `GrantRow`, `GrantInfo`, `GrantAAD`, `SealGrant`, `OpenGrant`, `CheckGrantShape` (the kit's) | `mailieSeal`, `Kind`, `kindName`, `grantRow`, `grantInfo`, `grantAAD`, `sealGrant`, `openGrant`, `checkGrantShape` (the kit's) |
| Platform wrap | `PlatformWrap`, `PlatformWrapBinding`, `CheckPlatformWrapShape`, `ValidProductKeyID`, `ErrPlatformWrap` (the kit's) | `mailiePlatformWrap`, `platformWrapBinding`, `sealMailiePlatformWrap`, `openMailiePlatformWrap` (the kit's) |
| Browser vault | `BrowserVaultAAD`, the vectors' reference (the kit's) | `mailieBrowserVault`, `browserVaultAAD`, `sealBrowserVault`, `openBrowserVault` (the kit's); `openBrowserVaultKey` (the console's) |
| Errors (section 13) | `ErrBinding`, `ErrShape`, `ErrPublicKey` (the kit's sentinels) | `MailieError`, `isMailieError` (the kit's) |
| Vectors | `vectors_test.go`, `testdata/`; `TestTheVectorsAreTheKitsFrozenOnes` holds `testdata/` to the kit's `vectors/mailie/key-scheme-v1`, byte for byte | `web/test/keyscheme.spec.ts` |
| Guarantees | `keyscheme_test.go` | `web/test/keyscheme.spec.ts` |

The kit modules underneath: Go `account`, `seal`, `hpke`, `platformwrap`, `profiles/mailie`,
`profiles/platform`, `jcs`, `vectors`; TypeScript `@thehappieco/kit/account`, `/seal`, `/hpke`,
`/platformwrap`, `/profiles/mailie`, `/profiles/platform/core`, `/browserAccount`, `/jcs`,
`/bytes`. The server's half of sections 11 and 12.1 to 12.6 (and of 12.7, in the release that
brought this scheme) is migration 0013
(`internal/store/migrations/0013_account_keys.sql`), `internal/auth/accountkeys.go` (the
ceremonies), `internal/service/users.go` and `internal/api/users.go` (the routes), the salt key in
`internal/store/saltkey.go`, and the reset invitation of `mailserver user password --bootstrap`.
The console's half is `web/src/crypto/account.ts` (the derivations and wraps of each ceremony),
`web/src/state/account.ts` (its requests, in order), `web/src/state/accountVault.ts` (the browser
vault, section 7) and the sign-in screens
(`web/src/components/SignInView.vue`, `RecoveryCodeDialog.vue`, `StepUpDialog.vue`), held by
`web/test/account.spec.ts`. The server's half of the mailbox keys and the grants (sections 8, 9 and
12.11 to 12.15) is migration 0014 (`internal/store/migrations/0014_mailbox_keys.sql`), the one
reader rule in `internal/store/readers.go`, the keys and grants written and read in
`internal/workspace/sealed.go`, the use cases in `internal/service/mailboxkeys.go` (with the link in
`accounts.go` and "read" with a grant in `workspaces.go`) and the routes in
`internal/api/mailboxkeys.go`. The console's half of them is `web/src/crypto/mailbox.ts` (a
mailbox's key pair and namespace, a grant sealed to the public key and seal id the server serves,
both checked first, and the person's own grant opened against the mailbox's public key at that
epoch; every mailbox private key zeroed once its grants are sealed),
`web/src/state/accountVault.ts` (`accountPrivateKeyOf`: the account key that opens grants, as the
kit's non-extractable private key), `web/src/state/grants.ts` (the person's own grant opened to
seal the key to someone else), `web/src/state/stepUp.ts` with `components/StepUpPrompt.vue` (the
step-up asked before every call of section 11, over whatever dialog is open),
`web/src/state/accounts.ts` (the link's key, section 12.11), `web/src/state/team.ts` ("read" with
a grant, section 12.13) and `web/src/state/mailboxKeys.ts` with `components/MailboxKeyPanel.vue`
(waiting for the key, supplying it, a first key from the sheet or right after a sign-in and never
on a restored session, section 12.14, and a personal mailbox's new key, section 12.12), held by
`web/test/mailboxKeys.spec.ts`, `web/test/mailboxKeys.page.spec.ts` and `web/test/accounts.spec.ts`
with the kit's real cryptography: every grant the console seals opens for its recipient, with
`openGrant`, to the key whose public half the server holds.

The server's half of the hosted sections (6.3, 11's hosted step-up, 12.8 and 12.10) is
provider-neutral, for an extension of the daemon to call: migration 0015
(`internal/store/migrations/0015_platform_wraps.sql`), `internal/auth/platformwraps.go` (the
ticket, the enrolment, the wrap answered and the read-only pin comparison) with
`internal/auth/identities.go` (the sign-in) and `accountkeys.go` (the step-up, and the reset that
deletes the wraps), and `internal/service/external.go` (`SignInExternal`, `EnrolExternal`,
`MarkExternalStepUp`, `ExternalStepUp`), held by `internal/auth/platformwraps_test.go` and
`internal/service/external_test.go`; the hosted service's extension adds only the provider's
protocol and its routes. The console's seams for it are `Edition.stepUp` and
`Edition.copy.stepUpHint` (`web/src/edition.ts`, asked by `web/src/state/stepUp.ts`) and
`adoptSession` (`web/src/state/session.ts`), which begins keyed when the edition's sign-in kept the
key of the person it names, held by `web/test/editionStepUp.spec.ts`.

## 17. Open questions

Decisions this specification left to the owner or to a later step, each without a byte that
depends on it. Those settled on 2026-10-09 say so.

1. **Where `K_salt` lives.** Settled: a server secret kept in the database under the credential
   sealer, as the send-hash root is, never an operator-provided variable.
2. **The hosted service's lost key.** What starts the replacement of section 12.6 when a person's
   platform wrap no longer opens (a new root at id.), and how a new product-key epoch is handled
   when the vault holds the account key (re-wrap) and when it does not. Settled for id. v1 on
   2026-10-10 (Appendix C): v1 has no key reset, so a root never changes for a `sub` and a lost
   root is a new id. account, which the operator meets by closing the old person (section 12.6);
   a person with an account key and no wrap at the pinned `product_key_id` (one a reset invitation
   made while id. was off, or a new epoch) is refused when a sign-in asks for the key (`no_wrap`),
   with no session, and closing and recreating them is the operator's way. A re-wrap under a new
   epoch waits until id. has one.
3. **The new-password minimum.** Settled by the owner: twelve code points, the platform's (section
   5.1), for new passwords only.
4. **The open console and the kit's names.** Settled: the console's sources import the kit by its
   package name, which `web/test/hosted.ts` sets aside as code, and the open console's build carries
   none of the platform profile's labels; a future import that would need them is a question for
   the owner first.
5. **The upgrade's oracle.** Settled by the owner: the legacy route and the `upgrade` answer exist in
   one release only and leave in the next (section 12.7), with the release notes saying so; the
   oracle lasts that long. They left in the next release (2026-10-10, Appendix C).
6. **What browsers remember of keys (phase 4).** Whether browsers pin the public keys they seal
   grants to (threat model, section 5.3) and remember a mailbox's keys across epochs (section 5.4):
   the defences left against a server or a database writer that substitutes a person's key or
   writes a mailbox a new epoch with a key of its own.
7. **The kit's Appendix D.** Settled on 2026-10-09: the kit's v0.7.0 took Mailie's profile, frozen
   from this specification's vectors, and its Appendix D records Mailie's `user_id` rule (the seal
   id, never the `sub`; section 6.1). The core uses the kit's profile since (section 16, Appendix
   C).

## Appendix A. The Mailie profile

| Value | |
|---|---|
| Account labels (HKDF info) | `mailie/v1/password/auth`, `mailie/v1/password/wrap`, `mailie/v1/recovery/wrap`, `mailie/v1/recovery/auth` |
| Password preparation | `thehappie-password/v1` (kit §11.2): presented, no minimum; new, 12 to 256 code points |
| KDF | `argon2id`, m 65536, t 3, p 1; bounds m 65536 to 262144, t 3 to 10, p 1 to 4, m × t at most 1048576, salt exactly 16 bytes |
| Address | `normalise`: Go's `strings.TrimSpace`, then Unicode's simple lowercase of each code point (Go's `strings.ToLower`) |
| Salt | `HMAC-SHA256(K_salt, "mailie/v1/kdf-salt\|" + normalise(address))[:16]`, the server's; an account stores only its target |
| Text encoding | base64url without padding |
| Recovery code | kit §6.7 generation; the platform's canonical form C (kit §11.6) |
| Account wrap | header `0x02`, 61 bytes, no legacy form; AAD `["mailie/account-wrap",1,kind,seal_id,base64url(public key)]`, kind `password` or `recovery` |
| Platform wrap | the kit's Mailie profile (kit Appendix D): header `0x03`, 61 bytes; salt `mailie/platform-wrap/v1`; label `mailie/platform-wrap`; `product_key_id` `mailie:<epoch>`; `user_id` the seal id, never the sub (a departure from kit §6.8) |
| Browser vault | the kit's JSON AAD (kit §8), not the restricted one: `["mailie/browser-account-key",1,seal_id,base64(public key)]` |
| Seal domain | magic `0x4d 0x4c` ("ML"), label `mlv1` |
| Kinds | section 10; grant `0x07` `mailbox_grant` |
| Grant | `GrantRow(namespace, namespace, seal_id, epoch)`; 88 bytes; epochs 1 to 65535 |
| Identities | seal id and namespace: lowercase UUIDv4 |
| Step-up | 10 minutes; self-hosted, from a verified sign-in or step-up on the server's clock; hosted, id.'s `auth_time` |

## Appendix B. Differences from Wappie's scheme

Mailie's scheme has Wappie's structure (an account key per person, wrapped under the password, the
recovery code and, through id., the product key; a key per archive, granted by sealing) and the
same kit underneath. It differs where Wappie's profile is frozen by data it already holds, and in
one rule of the kit itself, the platform wrap's `user_id` (section 6.1):

| | Wappie | Mailie | Why |
|---|---|---|---|
| Wrap AAD | the address, ECMAScript-lowered | kind, seal id, public key, as JSON | section 5.5 |
| Legacy wraps without AAD | opened as stale | none | no such data exists |
| Password preparation | none | the platform's | one password typed two ways is one password, in Go and every browser |
| KDF bounds | none | the platform's | a server cannot hand out cheap parameters (kit §6.2) |
| Text encoding | base64 | base64url | the platform's |
| Recovery code reading | Unicode upper case, `U` as `V` | the platform's canonical form | byte by byte, one grammar with id. |
| Salt | drawn by the browser; a decoy for unknown addresses | the server's for every address; an account keeps its address's | a challenge does not tell an account's salt from a decoy (section 5.3) |
| Platform wrap's `user_id` | the `sub` for an account made through id., its `users.id` for one linked (kit §6.8) | the seal id for every person | one rule for both editions (section 6.1) |
| Grant row | (tenant, device, user, epoch) | (namespace, namespace, seal id, epoch) | a mailbox has no separate device |
| Opening a grant | the plaintext is used as is | its public half must be the mailbox's | section 9.2 |
| Magic, label | `WS`, `wsv1` | `ML`, `mlv1` | section 10 |
| Browser vault tag | `wappie/browser-account-key` | `mailie/browser-account-key` | per product |

## Appendix C. Changes

- Version 1 (2026-10-09): first version, before any server or console code of phase 3. The same
  day, the owner settled the new-password minimum (twelve) and the upgrade's window (one release),
  and `K_salt`'s home and the open console's names were settled (section 17); no byte changed.
- Version 1, the server's half of sections 11 and 12.1 to 12.7 (2026-10-09). No byte changed. Where
  the code had to choose and this document did not, the narrowest choice, recorded here:
  - **`K_salt`'s envelope.** The meta row `kdf_salt_key`, sealed by the credential sealer for the
    purpose `auth/kdf-salt-key` with the ref `meta/kdf_salt_key` (a key service's policy must
    allow the purpose before a daemon that makes one runs under it), beside the send-hash root and
    under its rules: made at the first start, opened at every start or the daemon refuses to
    start, re-sealed by `rewrap-credentials`, replaced only when no configured key opens it
    (`rewrap-credentials --new-salt-key [--kms-key-lost]`), which moves every account off its
    target until its next sign-in. A daemon whose people sign in only through an extension makes
    none.
  - **Routes.** `POST /v1/auth/challenge`, `login`, `signup/open` and `signup`, `password/begin`
    and `password/finish`, `recover/open` and `recover/finish`, `recovery`, `stepup`,
    `upgrade/login` and `upgrade/enrol`, as section 12 names them; and `POST /v1/auth/reset/open
    {reset, email}` → `{salt, kdf, seal_id}` and `POST /v1/auth/reset {reset, email, auth_key, kdf,
    public_key, password_wrap, recovery_wrap, recovery_proof}` for section 12.6, "section 12.1 with
    the reset code instead of an invitation", whose link is `#reset=…&email=…`. `reset/open` is
    that ceremony's step 1: it answers the account's target, which the reset stores, where a
    challenge would answer an account off its target the salt it stores now. Every enrolment is
    told the seal id before it seals a wrap: `signup/open` (section 12.1) answers the one drawn for
    the invitation, which `signup` must name; `reset/open` and `upgrade/login` the person's own.
    `upgrade/login` answers `{ticket, seal_id, salt, kdf}`, never a session. `password/finish`
    answers the new session of a password change, and nothing (`204`) for a sign-in's
    re-derivation.
    `GET /v1/auth/me` names the person's `seal_id` and `public_key` and the session's
    `authenticated_at`, for the vault (section 7) and for asking for a step-up before a refusal;
    the console compares that time with the server's clock, as the `Date` of its answers gives
    it, never with its own.
  - **Tickets** carry the target they were issued with, and a finish stores that target, never one
    recomputed then (an address changed between the two halves would otherwise store a salt the
    browser did not derive with); `kdf` must be both the server's current default and the
    ticket's. A ceremony that changes a person's password deletes their other tickets; replacing
    the recovery code (section 12.5) deletes their recoveries opened with the old code, and leaves
    a password change or a re-derivation in flight, which proved the password it does not change.
  - **A password change's new session keeps the step-up time of the session it replaces**:
    section 11 does not list a change among what sets the time, so it opens no window of its own.
  - **The challenge** refuses something that is not an address (`bad_request`), as no account can
    have it, and spends the sign-in limit of the client's address only: it checks no secret and
    hashes nothing. Every other ceremony spends that and, when it names an account (its address,
    or the session's person), the account's: one budget per account, keyed by its stored address
    whichever way the request names it, so a sign-in, a step-up, a password change's first step and
    a recovery code's replacement draw on the same guesses.
  - **Errors.** A secret that does not verify is `unauthorized` on the public routes (`login`,
    `recover/open`, `upgrade/login`) and `not_authorized` on a session's (`password/begin`,
    `recovery`, `stepup`), never saying which part failed; a key, wrap, auth key or proof outside
    its shape is `bad_request`; parameters other than the current default are `conflict` (derive
    again); a ticket that is not valid, a step-up that is needed or refused, and a reset link that
    is not valid are `not_authorized`; a reset that would take the last reader is `conflict`.
  - **Verifiers** are hashed in the slots of people's secrets (two at once, the old passwords'),
    not those of API keys, so a burst of sign-ins cannot starve key checks.
  - **The seal id** of a new person is drawn by the code (`NewSealID`): for a sign-up by
    invitation, when the invitation is first opened, kept in `invites.seal_id` and refused at the
    sign-up unless the request names it (`409`, open the invitation again); a row written without
    one gets one drawn by the schema, as the migration draws them for the people who exist. The
    schema refuses changing a seal id, any change of `public_key` that does not move
    `key_replaced_at` forward in the same statement (the reset's), and any change of
    `zk_enrolled_at` once set; an enrolled row has every column of section 5.7 and no password
    hash (a CHECK).
  - **The reset invitation** replaces any earlier one of the person; it may be issued for a
    disabled person, whose link works only once they are enabled again, and disabling a person
    deletes their reset invitations and tickets. Completing it enrols a person who was not (an old
    password, or none, for a person who signs in through an extension) and clears any old
    password hash. Its last-reader test is section 12.6's, the last reader of any team mailbox, a
    team the person is alone in included (`workspace.LastReaderOfTx`), and not the one closing a
    person uses, which leaves such a team out because it goes with them: until mailboxes have keys
    (the next step of phase 3), that test is the flag alone, the rule of a mailbox without a key
    (section 12.13), so it refuses resets that would take "read" from nobody yet; `--force` goes
    ahead.
  - **The hosted step-up's mark** is a column of the session (`sessions.stepup_mark_at`); a new
    mark replaces one not yet used. A sign-in through id. whose `auth_time` is after the server's
    now takes now (section 12.8: "at most its own now"); a step-up whose `auth_time` is after now
    is refused (section 11).
  - **Replacing the recovery code presents the current auth key** (sections 11 and 12.5), where
    the first text asked for a fresh step-up only. Found in review: a sign-in opens the step-up
    window by itself, so a session token copied right after one could set a recovery code of its
    choosing, open a recovery with it and set the password (section 12.4), which the threat model
    (section 4.5) says a stolen session cannot. The window no longer guards it: the key in the
    same request does, under the account's sign-in limit, and is checked again in the transaction
    that stores the code. `recover/open` likewise checks again, in the transaction that issues the
    ticket, that the proof it verified is still the stored one, so a recovery opened while its
    code is replaced gets no ticket. No byte changed.
  - **A ticket that rides in an answer finishes only with the auth key that earned it** (sections
    12.2 and 12.3), where the first text named `password/finish {ticket, auth_key, kdf,
    password_wrap}`. Found in review: a sign-in's re-derivation ticket is in the same answer as
    the session, and a password change's in the answer to `password/begin`, so whoever saw only
    that answer (a proxy's log) could finish either with an auth key of their own and sign in as
    the person for good, which the threat model (sections 4.5 and 5.12) says a copied session
    cannot. The server keeps SHA-256 of the auth key it verified with the ticket
    (`auth_tickets.proof`), and `password/finish` carries that key again as `current_auth_key`;
    another is refused before any hash, and leaves the ticket unused. The console sends the key it
    derived for the sign-in or the first step. `recover/finish` and `upgrade/enrol` likewise
    refuse a ticket that is not theirs before the two hashes they cost, as `reset` does a code. An
    invitation's or a reset's address is compared with the request's as both are stored
    (section 2), byte for byte, never by a case fold that takes U+017F for an `s`. The console
    records an address as enrolled as soon as the server accepts a proof (section 12.7), not once
    the whole ceremony has succeeded, and a recovery whose sign-in after it fails is reported
    done. Section 7 now says what a browser closed while signed in keeps. No byte changed.
  - **A recovery's ticket finishes only with the recovery proof that opened it** (section 12.4),
    where the first text named `recover/finish {ticket, auth_key, kdf, password_wrap,
    recovery_wrap, recovery_proof}`. Found in review: the ticket rides in `recover/open`'s answer,
    so whoever saw only that answer (a proxy's log) could finish the recovery with a password and
    a code of their own, sign in as the person for good and lock them out. The server keeps
    SHA-256 of the proof it verified with the ticket (`auth_tickets.proof`, as for sections 12.2
    and 12.3), and `recover/finish` carries that proof again as `current_recovery_proof`; another
    is refused before any hash, and leaves the ticket unused. The console sends the proof it
    derived from the code typed. The upgrade's enrolment ticket (section 12.7, step 3) stays bound
    to no secret, a residual for the one release the route exists, now said there and in the
    threat model (section 5.12). An enrolment's answer (sections 12.1, 12.6 and 12.7) records the
    address before the console checks the key it names or keeps the account key, as section 12.7,
    step 2, says of every ceremony. No byte changed.
- Version 1, section 7's hosted rule made precise (2026-10-09). No byte changed. "A session that
  merely expires" had no test the console could run, and its flag kept the key past any session the
  page found no longer valid, a revoked one included. The key now stays only past an expiry a page
  established by the server's clock (its expiry timer, which now waits by that clock, or a
  refusal dated past `expires_at`, put to the server again when it carried no `Date` or the
  browser's clock called it expired), and that page marks the vault's record before clearing the
  session's; every other ending wipes it, and with no usable session record only a marked key stays;
  and an ending reaches only the record its session held, which the record now names
  (`Edition.accountKey.outlivesExpiry`, which replaces `outlivesSession`; the record's `holder`
  and `outlivedExpiry`; a record from before holders is settled by the first session that ends; the
  event stream's response now gives the page the server's clock too). Two residuals, both of
  storage: a page that loads with no session record while another tab is between keeping a new
  key and beginning its session wipes that key (the sign-in then holds it in its page only, and
  the next sign-in opens it again); and a browser that keeps refusing writes keeps whatever it last
  stored, a mark included. Keeping the key wherever the page found no session record, the rule's first
  draft, was found in review to keep a revoked session's key after a page stopped between clearing
  that record and wiping the key, or behind a browser clock running ahead.
- Version 1, the profile from the kit (2026-10-09). No byte changed. The kit's v0.7.0 publishes
  this profile, frozen from these vectors (its `profiles/mailie` in Go and TypeScript, kit
  Appendix D, and `vectors/mailie/key-scheme-v1`, the four files of `internal/keyscheme/testdata`
  byte for byte), and the core now uses it instead of its own copy: `internal/keyscheme` keeps its
  names as aliases and forwarders of the kit's, and `web/src/crypto/mailie.ts` re-exports the
  kit's module; only what the kit leaves to Mailie stays in the core (section 16). Go writes the
  same files with `-update`, TypeScript opens every case as before, and
  `TestTheVectorsAreTheKitsFrozenOnes` holds `testdata/` to the kit's copy. The profile's own
  refusals in the console are now the kit's `MailieError`, with the same codes (`binding`,
  `shape`, `vault`), in place of the console's `KeySchemeError`: the vault is wiped on one, and a
  ceremony takes one for a security error, as before (section 13).
- Version 1, the server's half of sections 8, 9 and 12.11 to 12.15 (2026-10-10). No byte changed.
  Where the code had to choose and this document did not, the narrowest choice, recorded here:
  - **The tables** (migration 0014, an ordinary one, with no view, so later rebuilds stay
    possible): `mailbox_keys` (mailbox, epoch, public key, namespace, `created_by`, `created_at`;
    one row per mailbox and epoch) and `mailbox_grants` (mailbox, its workspace, person, epoch, the
    88 bytes, `granted_by`, `created_at`; one per mailbox, person and epoch). The current epoch is
    the highest row of `mailbox_keys`, held nowhere else; older rows stay, written once, and only
    their grants go (section 12.12). Triggers refuse every update but blanking the attribution when
    that person is deleted, an epoch skipped or written twice, a second namespace for a mailbox or
    one another uses, and a key for an operator mailbox. A grant references its key row, the
    membership and the mailbox, each `ON DELETE CASCADE`, so the operator workspace, which has no
    members, holds none; the schema checks its length and magic, the code its whole shape. Every
    mailbox that exists at the upgrade stays without a key (section 12.14).
  - **Who reads** is one SQL rule in one place (`store.ReaderSQL`), which access, listing and
    fetching (`account.Visibility`), the caller's flags (`workspace.Access`), the last reader, the
    directory's reader count, sync eligibility and the team's consent all build on. `act` counts
    only where its holder reads. A member who holds the flag without a grant at the current epoch
    keeps the mailbox's card, is answered `access.read: false` and `access.waiting_key: true`, and
    counts as no reader; the access routes choose `403` or `404` by whether a grant row exists, not
    by the flags it reads to. A personal mailbox whose person lost their grant (a reset) keeps
    syncing under their consent until they write it a new key.
  - **Taking "read"** deletes the person's grants on the mailbox, every epoch's, in the
    transaction that takes the flag: a revoke (also one that leaves `send` or `manage`), a `PUT`
    without `read`, a membership disabled or removed, and their deletion. **Being disabled on the
    instance** deletes every grant of the person and keeps their flags, which count for nothing
    while they are off: switched back on, they wait for the key on every mailbox that has one. The
    **reset** (section 12.6) deletes every grant and keeps the flags; its last-reader test now
    counts only team mailboxes **that have a key**, where the reset takes "read", since on one
    without they keep reading by the flag; that narrows the step-3 choice recorded above, which
    refused resets that took "read" from nobody yet. After a forced reset the service tells the
    sync engine of every mailbox the person holds the flag on, and a team mailbox left with no
    reader stops syncing.
  - **Linking** (section 12.11): `POST /v1/accounts` carries `public_key`, `namespace` and `grant`
    flat, as section 12.11 spells them. From a person all three are required (`400` without them),
    each in its shape, the grant at epoch 1; from an instance key any of them is `400`. A person
    without `users.public_key` cannot link (`409`). The step-up is checked before the mail server
    is dialled and again in the transaction that creates the mailbox, which writes the key row and
    the linker's grant after the mailbox's row and the linker's flags, on the password and the
    OAuth paths alike; the console's margin for linking is two minutes, for the login check's
    thirty seconds. Resuming an abandoned link carries no key: the row has the one its link wrote,
    and a pending mailbox from before 0014 gets its first key by section 12.14.
  - **Giving "read"** (section 12.13): `PUT /v1/accounts/{id}/access/{user}` takes an optional
    `grant`, with the `public_key` it was sealed to (below), required when the change adds "read"
    on a mailbox that has a key for a person with a public key, and refused (`400`) with a change
    that adds no "read": the key goes to a member who holds the flag by the supply route. To a person without a public key, and on a mailbox
    without a key, "read" is the flag alone, with no step-up. The giver must read the mailbox by
    the rule (an owner or an admin waiting for the key gives nothing), and a grant for someone else
    takes a fresh step-up. The operator's `manage`-only change keeps working on a mailbox that has
    a key. Giving an API key "read" takes the same: the giver reads by the rule.
  - **Supplying the key**: `PUT /v1/accounts/{id}/grants/{user}` `{epoch, grant, public_key}` →
    the stored grant, from any person who reads the mailbox, after a fresh step-up, to an active
    member who holds the flag, has a public key and no grant at the current epoch.
  - **The first key** (section 12.14): `POST /v1/accounts/{id}/mailbox-key` `{public_key, namespace,
    grants: [{user_id, grant, public_key}]}`, whose grants must be exactly one for its writer and
    one for every other active member who holds the flag and has a public key: a missing or an
    extra one is `409`, so a console that raced an enrolment reads the mailbox key again. **A new
    key** (section 12.12): `PUT /v1/accounts/{id}/mailbox-key` `{epoch, public_key, grant}`, the
    namespace kept and not sent; a team mailbox is `403`.
  - **The key a grant was sealed to** (narrowing section 9.3). Every grant a browser posts for
    someone names the account public key it sealed it to: `public_key` (base64url, 32 bytes) beside
    `grant` when giving "read" and when supplying the key, and in each entry of a first key's
    `grants`, the writer's own included. The server refuses the grant (`409`) unless that key is
    the recipient's `users.public_key`, read in the transaction that writes it, and writes nothing
    of the request. Section 9.3 has the giver's browser seal "to the public key the server serves
    for the recipient", which leaves a console free to seal to one it read before the recipient's
    reset (section 12.6 replaces the key and keeps the seal id, so the binding still matches): that
    grant would count them a reader by the one rule, keep anyone from supplying them the key
    (they hold a grant at the current epoch), and never open. The server now refuses a grant whose
    stated key is not the recipient's current one; it still cannot check the bytes, which may seal
    anything to any key whatever the request names (section 9.3, threat model section 5.3). The
    console seals to the freshest answer it has, the members list read again just before it seals
    when giving "read" and the `waiting` entry of the mailbox key it reads in the same call when
    supplying it, and on a `409` reads again and tries once more. A `public_key` without a grant is
    `400`. A link's grant and a new key's are the writer's own, sealed to the key of the session
    that sends them, which a reset ends, and name none.
  - **What a console reads.** For a person signed in only: each account's `mailbox_key` `{epoch,
    public_key, namespace}` and `access.waiting_key`; `GET /v1/accounts/{id}/mailbox-key` for one
    who holds the flag (`403` for one who sees only the card), answering the key pair, their own
    grant at the current epoch, `waiting` (to a reader: the members to supply, with seal id and
    public key), `suppliers` (to a member waiting: who reads it) and `keyless_readers` (on a
    mailbox without a key: whom the first key is sealed to); the members list's `seal_id` and
    `public_key`; the directory's `epoch` and each grant's `sealed`. API keys, over REST and MCP,
    see none of it.
  - **Errors.** A public key, namespace, grant or epoch outside its shape, or a grant at odds with
    the epoch its own request names, is `bad_request`; a stale step-up `not_authorized`, as is a
    giver or writer who does not read; a mailbox the caller cannot see and a recipient who is not
    an active member `not_found`; a grant at another epoch than the current one, a grant that
    exists, a mailbox already keyed, a namespace in use, a first key's grants not matching, a
    grant that names another account public key than its recipient's now, a recipient without a
    public key or, for the key, without the flag, and a linker without a public key `conflict`.
  - **Left to phase 4**: a member whose grant exists but does not open (one forged by whoever held
    a session in its window) cannot be supplied, since the supply needs no grant at the current
    epoch; phase 4, when grants open content, decides how such a grant is replaced.
- Version 1, the upgrade removed (2026-10-10). No byte changed. The upgrade of section 12.7 existed
  in the release that brought this scheme only, as the owner settled with this document's first
  version (section 17), and leaves in the next; section 12.7 is its history now. What the code had
  to choose, the narrowest choice, recorded here:
  - **The server.** `POST /v1/auth/upgrade/login` and `/enrol` are gone (not found, as any path the
    daemon does not serve), with their service and the old hash's check (`LegacySignIn`, `Enrol`,
    and the 64 MiB Argon2id of a password in clear, its dummy and its byte limit): the server checks
    no password in clear anywhere. The challenge answers `{salt, kdf}`, never `upgrade`, and a
    person who never enrolled is answered the address's target, as an address without an account
    is; every sign-in of theirs fails with the one error, after one derivation against the dummy
    verifier (`TestEveryWayASignInFailsLooksTheSame`,
    `TestAChallengeAnswersAPersonWhoNeverEnrolledAsAnAddressWithoutAnAccount`, and over REST
    `TestAPersonWhoNeverEnrolledSignsInOverRESTOnlyAfterAResetLink`). The hashing slots of
    people's secrets, two at once, now hold 19 MiB each at most.
  - **The schema stays.** No migration: `users.password_hash` keeps a person's old hash until a
    reset clears it, read only for `has_password`; `auth_tickets`' CHECK still admits the purpose
    `enrol`, which nothing issues and no ceremony takes, so a ticket the release before issued
    finishes nothing and expires (`TestAnEnrolmentTicketLeftFromTheUpgradeFinishesNothing`).
  - **The way back** for a person who never enrolled is the reset invitation of section 12.6, which
    already enrolled a person who was not and cleared an old hash
    (`TestAPersonWhoNeverEnrolledSignsInAgainOnlyThroughAResetInvitation`); the release notes
    (`docs/self-hosting.md`) tell operators so, and the console tells a person still signed in from
    before the scheme to ask the administrator for one.
  - **The console** sends no password in any request: its sign-in derives and sends the auth key
    whatever the challenge answers, an `upgrade` member included, which it ignores; its strict
    contract check refuses one. The memory of enrolled addresses (section 12.7) defended only the
    upgrade, so nothing writes it any more: the browser vault's database stays at version 1, its
    `enrolled` store kept and cleared the first time a page of this release opens it, so that a
    page of the release before, still open in another tab, opens the database as it did and still
    wipes the account key at sign-out (a move to version 2 would have kept it from opening it, and
    the key at rest after its sign-out; found in review). Sections 12.1 to
    12.4 no longer record an address, and the error `upgrade_refused` is gone. The sign-in form,
    which never said a password stays in the browser while the upgrade could send one, now says so
    as the forms that choose a new password do. The earlier entries of this appendix that name the
    upgrade's routes, its ticket and the console's memory describe the release that brought this
    scheme.
- Version 1, the hosted half of sections 6, 11, 12.8 and 12.10 (2026-10-10). No byte changed. The
  owner decided that Mailie follows Wappie's structure where this document was silent, adapted to
  it (the seal id, never the `sub`, as the wrap's user id; section 7's hosted vault rule). The
  core's half is provider-neutral: it names no provider, and an extension of the daemon calls it
  (section 16). What the code had to choose, recorded here:
  - **Where the wraps live.** `platform_wraps` is the core's (migration 0015), since an extension
    reaches no store: one row per person and `product_key_id`, the wrap's shape and the id's
    spelling held by the schema too, insert only, deleted with the person or by the reset
    invitation (section 6.3). Section 6.3 was informative, and is normative now.
  - **What authorizes the first write** (section 12.10). A single-use ticket, as Wappie's, never
    the session: a first sign-in that asked for the key, of a person without `public_key`, opens no
    session, and is answered the ticket and the seal id; the enrolment `{ticket, public_key,
    platform_wrap, product_key_id}` writes both and opens the session. The ticket lives in a table
    of its own (`external_enrolments`), since `auth_tickets` holds a password target such a person
    does not have. A sign-in for the identity alone of a person without `public_key` is refused
    (`needs_key`, a `conflict` whose cause the extension tells apart) and creates and links
    nothing, so no session ever exists for a hosted person without an account key, and a session
    copied in the seconds before the first write cannot choose one. Two tabs that both sign in
    first: the first enrolment deletes the person's other tickets, and the second tab signs in
    again, which answers the wrap.
  - **How the server knows the page asked for the key** (section 12.8): the page says so (the
    extension passes `WantsKey`), since id.'s userinfo names the product key either way. The wrap is
    answered only then, and only at the `product_key_id` the extension pinned for the identity
    (checked again, read only, by the core).
  - **No wrap at the pinned id** (section 17.2): `no_wrap`, a `conflict` whose cause the extension
    tells apart, with no session. In id. v1 the root never changes for a `sub`, so this is a person
    whose account key a reset invitation made (id. turned off and on again); closing and
    recreating them is the operator's way. A re-wrap under a new epoch waits until id. has one.
  - **The step-up compares the product key with the pin** (section 11), read only, as Wappie's
    does: another key under the pinned id, an id nothing is pinned under, or no key named for an
    identity that has keys pinned is refused (a `conflict` whose cause is the changed key), logged
    as an error, the alert the extension raises for a sign-in, and changes nothing; a step-up never
    pins. An identity of a provider that delivers no product key, which has nothing pinned, steps
    up without one. The strict `auth_time` after the mark, in whole seconds, stays, against
    Wappie's minute of leeway before the start: id. and Mailie share one host clock in production.
    The step-up answers the session's new step-up time, which is `auth_time`.
  - **The words.** The refusals of a missing account key and of a stale step-up no longer tell a
    person to sign in or step up with a password: the console translates codes and never shows a
    message, and the hosted edition words them its own way.
  - **The console's seams.** An edition may bring its own step-up (`Edition.stepUp`), asked once
    for every flow waiting in place of the password dialog, with what the first flow asked it for
    (a link, a grant, a key), and its own words for the sentence beside a guarded write
    (`Edition.copy.stepUpHint`); its errors are worded by its describer, as before. An edition's
    sign-in that kept the account key it opened or made begins a keyed session (`adoptSession`
    asks the vault, as a restore does, and wipes a key of anyone else), so what follows a sign-in
    in the page that adopts it (the first keys of section 12.14, while the step-up time is fresh)
    follows it as it follows a password sign-in.
