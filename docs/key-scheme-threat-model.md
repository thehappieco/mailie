# Mailie's key scheme: threat model

- Covers: [`key-scheme.md`](key-scheme.md), version 1 (phase 3), cited as "spec §n", and the kit
  underneath it, whose own security considerations (kit SPEC §13) apply and are not repeated.
- Status: written with the specification, before any server or console code that runs it, for the
  external review. It says what each party can and cannot do, and what remains possible; where a
  defence holds only against part of an actor, it says which part.

## 1. In one paragraph

Phase 3 does **not** protect mail from the server: the index stays in clear, the server fetches
bodies and attachments live, and it holds every mailbox's credentials. What phase 3 does is take
the self-hosted password away from the server (it receives a derived auth key, never the
password), give every person an account key and every mailbox a key pair, and bind who may read a
mailbox to a sealed grant, so that phase 4 can seal content to the mailbox keys. Against a party
that copies the database or a backup, the keys hold: what it gets opens only with a person's
password (at the cost of Argon2id per guess), recovery code (150 bits) or id. secret. Against a
party that can write the database, they hold for what was sealed before it wrote, not after: it
can substitute a person's public key (section 5.3). Against a server that is itself malicious, the
scheme protects little in phase 3, and the most important reason is not cryptographic: the server
serves the console's code.

## 2. Assets

| Asset | Where it lives | Protected from |
|---|---|---|
| A self-hosted password | typed in the browser | the server, the database, backups, the network beyond TLS |
| `K_wrap`, the Argon2id master key | the browser, during a derivation | everyone but the page |
| The recovery code | written down by the person | everyone; the server sees only its proof |
| The account private key | the browser (in memory, and in the vault); wrapped in the database | the server, the database, backups |
| A mailbox private key | the browser (in memory); sealed in grants in the database | a copy of the database or a backup, people without a grant; not a party that writes the database (section 5.3) nor the server (section 5.2) |
| `sk_p` and id.'s root | id.'s page and Mailie's page, in memory | the Mailie server; the id. server |
| Who may read a mailbox | the "read" flag and a grant row | people without the right to give "read" |
| The public keys (a person's, a mailbox's) | the database, written once | the server's own bugs; not substitution by the server or a database writer (sections 5.3, 5.4) |
| **The mail itself** | the server's index, the provider, the server in transit | **not the server, in phase 3** (section 5.1) |
| Mailbox credentials | the database, under the server's sealer | outside this scheme (`docs/architecture.md`, `internal/secrets`) |

## 3. Assumptions

- **The console the browser runs is the code the server meant to serve, and is honest.** Every key
  of the scheme exists in clear only in that page. This is the assumption of every web application
  that encrypts in the browser, and the residual of section 5.2.
- TLS between the browser and the server, and between the server and id.
- The kit, the Go standard library, `@noble/hashes` and the browser's WebCrypto are correct; the
  kit's own considerations (kit §13: zeroisation is best effort, low-order points, engine quirks)
  hold.
- The server's clock, for the step-up's 10 minutes and the tickets' lifetimes; on the hosted
  service, id.'s `auth_time` for when a person last authenticated (spec §11).
- On the hosted service, id. behaves as the platform's protocol says (kit §11).

## 4. Actors

### 4.1 A malicious or compromised server

The Mailie daemon and its database, run maliciously or taken over: a self-hosted operator, or
whoever controls the hosted service's machine.

**Can:**

- read every message: it fetches them, indexes them in clear and holds the credentials (phase 3);
- serve a console that captures the password, the recovery code, the account key, `sk_p` and the
  mailbox keys as they are typed or opened (section 5.2);
- record, at a person's **first** enrolment, a public key other than the one the browser sent, show
  different people different public keys for one person, and, by writing its own database,
  replace a person's public key at any time (section 5.3);
- seal anyone a grant of a key of its choosing, and write the matching mailbox public key at a new
  epoch, since it controls its own schema (section 5.4);
- tell a browser that has not seen an account enrol that the account still needs the upgrade, and
  so receive the password (section 5.5);
- give two addresses one salt; withhold, corrupt or replay wraps and grants; change a seal id or a
  namespace. Each of these makes something fail to open: a denial of service.

**Cannot,** as long as the console it serves is honest:

- learn an enrolled person's password: it receives `auth_key`, an HKDF branch of a 64 MiB
  Argon2id of the password, never the password or `K_wrap` (spec §5.4);
- open a password or recovery wrap, a platform wrap or a grant, or make the browser derive with
  cheap parameters or a short salt: the browser refuses parameters outside the compiled bounds
  before deriving (spec §5.2; `account-go.json#account/derive/refuses/*`);
- plant an account key through the platform wrap: it is symmetric under `sk_p`, which the server
  never holds, and its opener checks the opened key against the public key (spec §6.1);
- make a person's own browser accept a wrap of a key other than the one bound: the wrap's AAD
  binds the public key, and the opener checks the opened key's public half (spec §5.5;
  `account-go.json#mailie/account-unwrap/refuses/opens-to-another-accounts-key`);
- make a grant open to a key other than the mailbox's **at the public key it serves for that
  epoch** (spec §9.2; `grant-go.json#mailie/grant-open/refuses/a-key-the-sealer-chose`);
- present one person's grant as another's, one mailbox's as another's, one epoch's as another's,
  or one product's envelope as Mailie's (spec §9.1, §10, §6.1).

### 4.2 id., the platform's identity provider (hosted service)

**Can:** sign anyone in to the hosted service, since it is the identity provider, and so obtain a
session and everything a session reaches (section 4.5), which in phase 3 includes the mail through
the server; as the page that unlocks the root, capture a person's id. password and root, and so
derive `sk_p`; with `sk_p` **and** a copy of Mailie's database, open the person's platform wrap,
their account key, their grants and their mailbox keys.

**Cannot:** open anything of Mailie's with `sk_p` alone (the wraps are in Mailie's database); see
an account key or a mailbox key from its own server, which relays `sk_p` sealed to Mailie's page
(kit §11.12); replace a person's pinned product key for an epoch Mailie has pinned (kit §11.15:
the pin is insert only, and a different key is refused); make a platform wrap open to an account
key other than `users.public_key`.

A first sign-in, and a new product-key epoch, trust id.'s key registry (kit §11.15).

### 4.3 A member of a workspace

**Can:** read the mailboxes they read by spec §12.13 (the flag, and a grant once the mailbox has a
key); keep a copy of a mailbox key their browser opened; supply that key, after a step-up, to a
member who already holds the flag on that mailbox (spec §12.13).

**Cannot:** read a mailbox they hold no "read" on (the service's authorization, unchanged); give
themselves or others "read" (the service accepts a grant to someone without the flag only from an
owner or an admin who reads the mailbox now, spec §9.3; supplying the key to someone who already
holds the flag gives nobody "read"); open another person's grant (sealed to another key,
`grant-go.json#mailie/grant-open/refuses/another-persons-key`) or present it as theirs
(`grant-go.json#mailie/grant-open/refuses/bobs-grant-as-alices`).

A member whose "read" is taken keeps whatever their browser already opened, the mailbox key
included (section 5.7).

### 4.4 An owner or an admin of a workspace

**Can:** everything a member can; give "read" on a mailbox they read, to any active member, after
a step-up; take it; remove a mailbox.

**Cannot:** read a mailbox by their role (unchanged); give "read" on a mailbox they do not read: the
service refuses, and they could not seal the key anyway, since they hold none; take a team mailbox
by giving it a key of their own: a team mailbox is never re-keyed, and only a person who reads a
mailbox that has no key may write its first key, with a step-up (spec §12.12, §12.14). A team
mailbox nobody can open is removed and linked again, which takes the provider's consent.

### 4.5 A stolen session

A session token copied out of a signed-in browser, a log or a proxy. It is a bearer for up to 14
days (less on the hosted service).

**Can:** everything the person can do through the server without proving who they are: in phase 3
that includes reading their mailboxes through the server, and acting and sending as their flags
allow; ask for their grants and platform wrap, which open nothing without their account key.

**Can, if it was copied within 10 minutes of the session's sign-in or last step-up** (spec §11):
also what the step-up guards, until those 10 minutes end: give an accomplice "read" with 88 bytes
of the right shape, write a mailbox key, the first key of a mailbox that has none included (a key
it chose, which a team mailbox keeps for good, spec §12.12 and §12.14), and replace the recovery
code. On the hosted service the 10 minutes are counted from id.'s `auth_time`, so a silent sign-in
opens none (section 5.12).

**Cannot:** obtain the password wrap (it is answered only to an auth key verified in the same
request, spec §5.7) or the recovery wrap; change the password (it needs the current auth key);
after those 10 minutes, do any of what the step-up guards (spec §11). A step-up proves the
session's own person: the self-hosted one checks an auth key against that person's verifier only,
and the hosted one refuses an id. sign-in whose issuer and `sub` are not the identity linked to
that person, so signing in as oneself does not step up someone else's session. Without the
step-up, a stolen owner's session could give an accomplice "read" with 88 bytes of the right
shape, because in phase 3 the server checks that a grant exists, not that it opens.

### 4.6 A stolen device

A browser profile, unlocked, or a copy of its files.

**Can,** while the person is signed in: everything a stolen session can, and use the account key
the vault holds. Non-extractable means that no script reads the vault's AES key out of the
browser; it does not mean the key is absent from the profile's files, and whether a copy of those
files carries a usable key depends on the browser and the operating system. The scheme assumes it
does: **a copied profile of a signed-in person holds their account key.**

**After the session ends,** it depends on the edition (spec §7):

- **Self-hosted:** the console wipes the vault at sign-out and whenever it finds no valid session,
  an expired one included, since every sign-in opens the password wrap anyway. A browser whose
  session ended holds nothing that opens without the password.
- **Hosted:** the vault outlives a session that merely expires, so that the next sign-in needs no
  product key, and id.'s own session in the same browser may sign the person in again silently:
  signing out of Mailie sends the person to id.'s sign-out, which asks whether to end id.'s
  session and never ends it by itself. Whoever uses a browser left with a live id. session can do
  everything a signed-in device can, but not what the step-up guards: id.'s `auth_time` is as old
  as the person's last real authentication (section 5.12). Once the person has signed out of
  Mailie (which wipes the vault) and of id., nothing signs in silently.

**Deleted is not erased.** Wiping the vault deletes its IndexedDB record, and the browser no longer
opens it; but the record (the AES key's bytes and the 48-byte ciphertext) may stay in the
profile's files until the browser compacts its storage (Chromium's IndexedDB is LevelDB, where a
delete writes a tombstone), and on an unencrypted disk after that. A forensic copy of the profile
taken after sign-out may still yield the account key. Full-disk encryption is the person's remedy
against that.

Changing the password does not change the account key. The remedy for an account key that may have
been copied is the reset of spec §12.6, which replaces it and deletes the grants sealed to it,
followed by new keys for the person's personal mailboxes; it is refused without force while the
person is the last reader of a team mailbox, which another reader should be given first.

### 4.7 A backup thief

A copy of the database or of a backup: the self-hosted operator's, or the hosted service's
encrypted backups together with the key that decrypts them.

**Gets:** the **index in clear** (phase 3); the credentials, under the server's sealer, whose key
it does not get from the database; every seal id, public key, salt, KDF parameter set, verifier,
wrap and grant.

**Can:** guess a person's password offline. Each guess costs one Argon2id at the account's
parameters (64 MiB, three passes by default) and a decryption of the password wrap, or a further
19 MiB Argon2id against the verifier; the verifier is no cheaper an oracle than the wrap
(spec §5.7). A weak password falls. Until a person upgrades, the old server-side hash (Argon2id,
64 MiB) offers the same cost; a backup taken before an upgrade keeps it until the backup expires.

**Cannot:** use a recovery wrap without the code (150 bits), a platform wrap without `sk_p`, or a
grant without the account key; replay a verifier as an auth key (it is a hash of it).

### 4.8 A network attacker

TLS stands between them and everything. An auth key captured in transit signs its holder in as the
person, as a password did, and yields the password wrap, which still needs the password to open:
the attack becomes the backup thief's offline guessing, for one account.

### 4.9 Another product of the platform

Wappie runs the same kit with its own profile. A Wappie envelope fails at its magic, a Wappie
platform wrap at its labels and its product key, and no label is shared
(`grant-go.json#mailie/grant-open/refuses/a-wappie-device-grant-of-the-same-binding`,
`platform-wrap-go.json#mailie/platform-wrap/open/refuses/the-same-persons-wappie-wrap`,
`TestNoLabelOrHeaderIsAnotherProfiles`).

### 4.10 An API key or an MCP client

Unchanged in phase 3: a key reads through the server what its workspace's owners gave it
(`key_access`), and holds no account key. Phase 4 decides service identities.

## 5. Residuals

### 5.1 The server reads the mail

In phase 3 the server fetches, indexes and serves every message in clear; nothing in this scheme
changes that, and no text should say otherwise. Phase 4 seals the index to the mailbox keys; even
then the server fetches new mail from the provider, and sees it as it arrives, because it holds the
credentials.

### 5.2 The server serves the console

Every secret of the scheme exists in clear only in a page the server served. A server that serves
a modified console receives the password as it is typed, the recovery code, the account key, `sk_p`
and every mailbox key. The zero-knowledge property therefore holds against a copy of the database
or a backup, and against a server that stays honest about its code; it does not hold against the
server's operator acting maliciously. Against a party that can write the database but not the
served code it holds only in part: such a party gets no password and no key already sealed, but it
can substitute a person's public key and receive every mailbox key sealed to that person afterwards
(section 5.3). On a self-hosted server the operator controls the code and the data alike. On the
hosted service the code is the hosted service's. The page's strict content security policy narrows
what other script can do; it does not narrow what the server can serve.

### 5.3 Key substitution

`users.public_key` is written once, at the person's own enrolment, and the person's browser checks
its own key at every sign-in (the wrap's AAD binds it). But a giver's browser seals to the public
key the server shows it for the recipient, and nothing in phase 3 lets it check that key against
the recipient's own view.

- **The server** can record a key of its own at a first enrolment, or show givers a key the
  recipient never saw, and receive the mailbox keys sealed to it.
- **Whoever can write the database** can do the same at any time. "Written once" is the schema's
  rule (spec §3.1, §4): triggers in the same SQLite file, which stop the server's own code from
  changing a key by mistake and nothing more. A party with the database file, or with arbitrary
  SQL through the server, drops the trigger as easily as it writes the row: it replaces Bob's
  public key with its own, the next owner who gives Bob "read" seals the mailbox key to it (spec
  §12.13), and it reads that grant back from the database and opens it. Bob's browser would notice
  at his next sign-in, when his wrap no longer opens under the substituted key; but a writer that
  holds the mailbox key can put Bob's key back and write him a proper grant first, since the
  grant's written-once rule is the schema's too.

In phase 3 this exposes no mail that the database does not already hold in clear; in phase 4 it
would expose everything sealed to that mailbox key. A browser-side pin (each browser recording the
keys it sealed to and its own person's, and refusing a changed one until a fingerprint is compared
out of band), or a log of keys, would narrow it; neither is in phase 3, and phase 4 decides (spec
§15, §17). Wappie has the same residual.

### 5.4 Grants are not authenticated

HPKE's base mode authenticates no sender (kit §11.12 says the same of key delivery): anyone who
knows a person's public key can seal them a grant, rightly bound, of a key of their choosing. The
opener's check that the key's public half is the mailbox's public key at that epoch (spec §9.2) is
the mitigation: the mailbox key is written once per epoch by the linker's browser, so a forger must
also write that row. A member, an API key, a database writer that leaves that row alone, and a bug
cannot; the server and any database writer can, by writing a new epoch with a key of their own
(section 5.3). So can a person's own session for a mailbox that has no key yet, whose first key a
reader writes (spec §12.14): the step-up is what keeps a stolen session from doing it outside its
10 minutes (section 5.12). In phase 3 nothing is sealed to a mailbox key, so such a key reveals
nothing. In phase 4 it would matter for what browsers seal to the mailbox (drafts, a search
index); phase 4 must decide whether browsers remember a mailbox's keys across epochs (spec §17).

### 5.5 The upgrade sends the password once

A person who signed up before phase 3 sends their password in clear to the server one last time
(spec §12.7). A browser remembers every address that enrolled or proved a zero-knowledge secret in
it (signing up, signing in, a password change, a recovery, a reset, the upgrade, a step-up), keyed
by the server's origin and the address as the server stores it, so another spelling of the address
is the same record; it never sends that address's password again. A browser that did not (a new
device, cleared storage, a browser only ever used before phase 3) cannot tell an account that needs
the upgrade from a server that pretends it does, and would send it. Until the legacy route is
removed, a challenge also says to anyone that an address has an active account not yet upgraded
(a disabled one is answered as an unknown address): the `upgrade` answer is an enumeration oracle
for those accounts, under the sign-in rate limits. Both end when the route is removed in a later
release (spec §17).

### 5.6 Offline guessing from a copy

Section 4.7: a copy of the database lets its holder guess each password at the cost of a 64 MiB,
three-pass Argon2id per guess. A later release can raise the default, which every account takes at
its next sign-in (spec §5.3); the bounds keep any server from lowering it. A copy taken before then
keeps the old cost.

### 5.7 Revocation does not take back what was opened

Taking "read" deletes the person's grants (spec §12.13), but a browser that opened a grant has seen
the mailbox key, and a copy of it opens anything sealed to that epoch. Phase 3 seals nothing to it.
Only a personal mailbox is re-keyed (spec §12.12); a team mailbox keeps its key, and phase 4 decides
whether taking "read" re-keys it and seals again for the others (Wappie has not built that either).

### 5.8 A person who loses everything

A self-hosted person who loses both their password and their recovery code, and a hosted person
who loses both id.'s password and its recovery code, lose their account key. The operator's reset
(spec §12.6) gives a new key and deletes the old grants: the person's personal mailboxes are
re-keyed, and a reader supplies them the key of a team's again. The reset is refused without force
while the person is the last reader of a team mailbox, since deleting their grant would leave it
with no reader, and a team mailbox is never re-keyed; with force (the key is truly lost, and the
grant opens nothing anyway), such a mailbox can only be removed and linked again. In phase 4 a
re-keyed mailbox is synced again from the provider; nothing sealed under the old key is
recovered.

### 5.9 Salts

The server derives every salt from the address as it stores it, under its own secret, and keeps
each account's stored salt and parameters at its target, what an unknown address would be answered
(spec §5.3), so a challenge answers the same for an address before and after its account exists.
Two windows break this, each until the person's next sign-in re-derives (spec §12.2, step 5):

- **After an address change**, the new address still answers the old address's salt, which anyone
  gets by asking for the old one. Whoever knows or guesses the old address can tell that the new
  one has an account and that it was the old one's, and an account later made at the old address
  shares the salt until then.
- **After a raise of the default parameters**, an enrolled account still answers the old ones,
  which tells it apart from an unknown address.

An account whose person never signs in again stays in its window. The browser cannot tell two
addresses given one salt (kit §13).

### 5.10 Metadata

The server knows who holds a grant on which mailbox, when grants were given and by whom, every
public key and seal id: it decides access, so it knows anyway. The scheme hides contents, in phase
4, not the shape of who reads what.

### 5.11 Best effort in the page

Zeroising keys in JavaScript and Go is best effort (kit §13): strings cannot be cleared, and
runtimes copy buffers. A page compromised by script runs with every key it holds (section 5.2). A
deleted browser record is not erased from disk until the browser compacts its storage (section
4.6).

### 5.12 The step-up window

A sign-in counts as a step-up for 10 minutes (spec §11), so that the console can key mailboxes
right after it. A session token copied within those 10 minutes (from a logging proxy that saw the
sign-in's answer, or a device taken right after a sign-in) can do what the step-up guards: give an
accomplice "read" with grant-shaped bytes, write a mailbox key of its choosing, the first key of a
keyless mailbox included, and replace the recovery code. On a self-hosted server the window opens
at a sign-in or a step-up that verified the person's secret; on the hosted service it opens only
when id. says the person authenticated (`auth_time`), so a silent sign-in from id.'s session opens
none. Not counting a sign-in as a step-up would close the window at the cost of a second password
prompt after every sign-in that keys mailboxes.

## 6. Where each defence is tested

| Defence | Vectors | Tests |
|---|---|---|
| Bounds before derivation | `account-go.json#account/derive/refuses/*` | `TestTheProfileDerivesOnlyWithinThePlatformsBounds` |
| One password, one key, in Go and the browser | `account-go.json#account/derive/nfc`, `account-go.json#account/derive/nfd-with-no-break-spaces` | `web/test/keyscheme.spec.ts` |
| A wrap opens for its person, kind and key only | `account-go.json#mailie/account-unwrap/refuses/*` | `TestAnAccountWrapOpensOnlyForItsPersonKindAndKey` |
| A recovery code read byte by byte | `account-go.json#account/normalise/refuses/*` | |
| Products apart | `platform-wrap-go.json#mailie/platform-wrap/open/refuses/*`, `grant-go.json#mailie/grant-open/refuses/a-wappie-device-grant-of-the-same-binding` | `TestThePlatformWrapRunsTheKitsMailieVectors`, `TestNoLabelOrHeaderIsAnotherProfiles` |
| The platform wrap binds the seal id, never the sub | `platform-wrap-go.json#mailie/platform-wrap-binding/refuses/seal-id-the-sub`, `platform-wrap-go.json#mailie/platform-wrap/open/refuses/the-sub-as-the-user-id` | `TestThePlatformWrapsUserIDIsTheSealIDNeverTheSub` |
| A grant opens for its person, mailbox, epoch and kind only | `grant-go.json#mailie/grant-open/refuses/*` | `TestAGrantOpensOnlyToTheMailboxsKeyForItsPersonMailboxAndEpoch` |
| A grant the sealer chose does not open | `grant-go.json#mailie/grant-open/refuses/a-key-the-sealer-chose` | same |
| No grant to a low-order key; the server stores none | `grant-go.json#mailie/grant-seal/refuses/low-order-recipient/*`, `grant-go.json#mailie/public-key-check/refuses/*` | `TestAGrantIsNeverSealedToALowOrderKey` |
| The server's shape checks | `account-go.json#mailie/account-wrap-shape/*`, `grant-go.json#mailie/grant-shape/*` | |
| The vault binds the person and the key, and one code refuses a record that does not open | `browser-vault-go.json#mailie/browser-vault/aad/*` | `TestTheBrowserVaultBindsTheSealIDNotTheAddress`, `web/test/keyscheme.spec.ts` |
| One spelling of an address, in Go and the browser | `account-go.json#mailie/normalise-address/*` | `TestTheSaltIsOfTheAddressAsTheServerStoresIt`, `web/test/keyscheme.spec.ts` |
| Salts tell nothing outside the windows of section 5.9 | `account-go.json#mailie/decoy-salt/*` | `TestTheSaltIsOfTheAddressAsTheServerStoresIt` |

The guarantees that need the server or the console's ceremonies are tested with the code that
enforces them, in the next steps of phase 3: written-once columns; the step-up, that it proves
the session's own person (a step-up as another person or another id. identity is refused), that a
silent sign-in does not freshen it, and that it guards every key written, the first key of a
keyless mailbox included; who may give "read" and supply the key; the upgrade's one-way flag and
the browser's memory of enrolled addresses under any spelling; the targets of salts and
parameters; the deletion of grants, and the reset's last-reader guard. The specification's
sections 11 and 12 are what those tests hold the server and the console to.
