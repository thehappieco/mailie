# Self-hosting

Mailie runs as one process: a static binary and one SQLite database in a data directory. It has
no web screen and no people: you run it from its command line, and your tools use its REST API
with keys you make there. It serves no MCP of its own ([No MCP](#no-mcp)). This page is how to run
it on a server of your own, from a binary under systemd or in a container, behind a reverse proxy
that terminates TLS. The packaging is in [`deploy/`](../deploy): a `Dockerfile`, a `compose.yaml`
and example systemd units.

- [Requirements](#requirements)
- [The configuration a server needs](#the-configuration-a-server-needs)
- [With Docker Compose](#with-docker-compose)
- [With Docker alone](#with-docker-alone)
- [From a binary, with systemd](#from-a-binary-with-systemd)
- [TLS through a reverse proxy](#tls-through-a-reverse-proxy)
- [The first key](#the-first-key)
- [Mailboxes and OAuth clients](#mailboxes-and-oauth-clients)
- [No MCP](#no-mcp)
- [Health, metrics and logs](#health-metrics-and-logs)
- [Backups](#backups)
- [Credentials under AWS KMS](#credentials-under-aws-kms)
- [Upgrades](#upgrades)
- [No people, and no passwords](#no-people-and-no-passwords)

## Requirements

- **A Linux machine** (amd64 or arm64) that can reach your mail providers: IMAP on 993, SMTP on
  465 or 587, and their OAuth endpoints on 443. From outside, only the reverse proxy's 443 (and 80)
  need to be open, and only for tools on other machines.
- **A name and a certificate**, for scripts and tools on other machines: they reach the
  daemon through a reverse proxy that terminates TLS (Caddy, nginx); the daemon itself only speaks
  plain HTTP on loopback. Tools on the server's own machine reach it there.
- **To build it:** Docker with BuildKit (any current Docker Engine or Docker Desktop) builds the
  binary in the image. Without Docker, Go 1.27. Neither needs Node: the server has no web console.
- **Disk** for the index, which holds metadata only (never bodies or attachments), and for the
  download spool: an attachment or an original being downloaded is held whole under the data
  directory, at most `MAIL_DOWNLOAD_SPOOL_MAX_BYTES` (1 GiB by default) at once.

Mailie has no stable release yet: you build it from a checkout of this repository.

```sh
git clone https://github.com/thehappieco/mailie.git && cd mailie
```

## The configuration a server needs

Configuration is environment variables only; [`.env.example`](../.env.example) documents each one.
A server needs at least these:

| Variable | On a server |
|---|---|
| `MAIL_CREDENTIAL_KEY_HEX` | Required, unless an AWS KMS key seals the credentials instead ([below](#credentials-under-aws-kms)). `openssl rand -hex 32`. It encrypts every stored refresh token and password, and the send-hash root. **Keep a copy with your other secrets: losing it means authorizing every mailbox again** ([below](#the-send-hash-root-is-kept-in-the-database)). |
| `MAIL_ENV` | `prod`: logs in JSON, and refuses the settings that would leak a token. |
| `MAIL_TRUSTED_PROXIES` | The address the daemon sees your reverse proxy connect from (see [below](#the-proxys-address)). Without it every client shares one rate limit. |
| `MAIL_GOOGLE_CLIENT_ID`, `_SECRET`; `MAIL_MICROSOFT_CLIENT_ID` | The installed OAuth clients, for Gmail and Microsoft mailboxes ([below](#mailboxes-and-oauth-clients)). iCloud and IMAP mailboxes need none. |

`MAIL_PUBLIC_URL`, the web OAuth clients (`MAIL_GOOGLE_WEB_CLIENT_*`,
`MAIL_MICROSOFT_WEB_CLIENT_*`) and `MAIL_WEB_DIR` are for an edition that serves a web page: a
self-hosted server leaves them unset (["The open console leaves"](#the-open-console-leaves), under
Upgrades, says what to do with the ones an older release needed), and so it leaves
`MAIL_PASSWORD_SIGN_IN`: off, the default, the server has no people
([below](#no-people-and-no-passwords)). Two more say where things are: `MAIL_DATA_DIR` (the
database) and `MAIL_HTTP_ADDR` (what the daemon listens on). The image and the systemd unit set
them; leave them out of your environment file.

## With Docker Compose

[`deploy/compose.yaml`](../deploy/compose.yaml) builds the image from your checkout and runs it
with its root file system read-only, no capabilities, `no-new-privileges`, the database on a named
volume (`mailie_data`) and the port published on the host's loopback only (`127.0.0.1:8080`). It
keeps `MAIL_WEB_DIR` empty, whatever `deploy/.env` says: the daemon serves the API only.

```sh
cp .env.example deploy/.env
chmod 600 deploy/.env
# Edit deploy/.env: MAIL_CREDENTIAL_KEY_HEX, MAIL_ENV=prod, MAIL_TRUSTED_PROXIES, the OAuth clients.
cd deploy
docker compose build
docker compose run --rm mailie apikey create --bootstrap --scope admin --name cli   # the first key
docker compose up -d
curl -fsS http://127.0.0.1:8080/v1/healthz
```

`deploy/.env` holds the credential key: Git ignores it, and it never enters the image (the build
context lets in only `go.mod`, `go.sum`, `cmd/` and `internal/`). Compose reads the file itself, so
write values without quotes unless a value holds a `$`, and then in single quotes.

The rest of this page writes the commands from `deploy/`:

| What | How |
|---|---|
| A command that talks to the running daemon (`account`, `user`, `workspace`, `member`, `access`, and `apikey create\|list\|revoke` with `MAIL_ADMIN_API=true`) | `docker compose exec -e MAIL_ADMIN_KEY mailie mailserver account list`, with `MAIL_ADMIN_KEY` in your shell |
| A command that needs the daemon stopped (`--bootstrap`, `migrate`, `rewrap-credentials`) | `docker compose stop mailie`, then `docker compose run --rm mailie <command>`, then `docker compose start mailie` |
| The logs | `docker compose logs -f mailie` |

`docker compose exec` gives the command your terminal, so `account add --password -` asks for the
password at a prompt that does not show it; to pipe one in instead, add `-T`
(`printf '%s\n' "$APP_PASSWORD" | docker compose exec -T -e MAIL_ADMIN_KEY mailie mailserver account
add --email you@icloud.com --password -`).

`apikey create`, `apikey list` and `apikey revoke` call routes the daemon mounts only with
`MAIL_ADMIN_API=true` in its environment (`deploy/.env`; off by default, and the commands then say
so). Without them, `apikey create --bootstrap` issues a key with the daemon stopped; listing and
revoking instance keys need the routes.

The image builds for the machine it is built on. To build on one machine and run on another, set
the platform: `docker build --platform linux/amd64 -f deploy/Dockerfile …`.

## With Docker alone

The same image without Compose, from the checkout's root, with the same protections:

```sh
make image          # or: docker build -f deploy/Dockerfile --build-arg VERSION="$(git describe --always --dirty)" -t mailie:local .
docker volume create mailie_data
docker run -d --name mailie --env-file deploy/.env \
  -p 127.0.0.1:8080:8080 -v mailie_data:/data \
  --read-only --tmpfs /tmp --cap-drop ALL --security-opt no-new-privileges:true \
  --restart unless-stopped --stop-timeout 30 mailie:local
```

`docker run --env-file` takes every value literally, quotes and comments after it included: write
values bare, comments on lines of their own. Keep `MAIL_DATA_DIR`, `MAIL_HTTP_ADDR` and
`MAIL_WEB_DIR` out of that file, so the image's (`/data`, `0.0.0.0:8080`, and no web directory)
apply.

What the image is:

- **Two stages.** Go 1.27 builds a static binary (`CGO_ENABLED=0`, `-trimpath`, the version
  stamped in), and it is copied onto distroless's `static-debian13` image, which holds CA
  certificates, time zones and little else: no shell, no package manager. Every base image is
  pinned by digest. The image is the daemon alone: REST, with no MCP, no web console and no
  `MAIL_WEB_DIR`; `GET /` and `/mcp` answer 404.
- **It runs as `nonroot` (65532:65532)**, and the binary belongs to root. `/data` is the only place
  it writes, besides `/tmp`; it is `0700` and the daemon's, and a new named volume takes that owner
  and mode. A bind mount needs the same: `chown 65532:65532` and `chmod 700` on the host
  directory.
- **No secret is built in.** Everything comes from the container's environment when it starts.
- **No `HEALTHCHECK`:** the image has no tool to run one. Check from outside (see
  [Health](#health-metrics-and-logs)).
- On `SIGTERM` the daemon closes its mailbox connections and exits within 15 seconds; Compose and
  the command above allow 30.

## From a binary, with systemd

Build the binary on any machine with Go 1.27. It is static, so it can be built elsewhere for the
server's platform:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --always --dirty)" -o dist/mailserver ./cmd/mailserver
```

Or take it out of the image (`docker build --platform linux/amd64 …` for another machine):

```sh
mkdir -p dist
docker create --name mailie-files mailie:local
docker cp mailie-files:/usr/local/bin/mailserver dist/mailserver
docker rm mailie-files
```

On the server, as root, with [`deploy/mailie.service`](../deploy/mailie.service):

```sh
useradd --system --user-group --home-dir /var/lib/mailie --no-create-home --shell /usr/sbin/nologin mailie
install -d -o mailie -g mailie -m 0700 /var/lib/mailie
install -m 0755 dist/mailserver /usr/local/bin/mailserver
install -d -m 0700 /etc/mailie
install -m 0600 .env.example /etc/mailie/mailie.env
# Edit /etc/mailie/mailie.env: MAIL_CREDENTIAL_KEY_HEX, MAIL_ENV=prod, MAIL_TRUSTED_PROXIES, the OAuth clients.
install -m 0644 deploy/mailie.service /etc/systemd/system/mailie.service
systemctl daemon-reload
```

The unit runs the daemon as `mailie` with the database in `/var/lib/mailie`, listening on
`127.0.0.1:8080`, and serves the API only: it sets no `MAIL_WEB_DIR`. The environment file belongs
to root, `0600`: systemd reads it before the daemon starts, so the daemon's user cannot read the
file itself. systemd reads that file without a shell: one `NAME=value` per line, and no comment
after a value.

The unit is hardened: no capabilities, a read-only system (`ProtectSystem=strict`) with only its
state directory writable, a private `/tmp`, no devices, no other users' homes, a system-call filter,
only IPv4, IPv6 and Unix sockets, and `MemoryDenyWriteExecute`. Its paths are examples; change them
together.

Every other command runs as `mailie` with the unit's environment, which `systemd-run` reads from
files, as the unit does. Secrets go only in files: a value given to `systemd-run` with `-E` or `-p
Environment=` becomes a property of the transient unit, which any local user can read over D-Bus
while the command runs (systemd.exec(5) says not to put secrets there), while `EnvironmentFile=`
publishes only the file's path. So the admin key the commands that talk to the daemon need has a
file of its own, root's and `0600`, with one line, `MAIL_ADMIN_KEY=<the key>`:

```sh
sudo install -m 0600 /dev/null /etc/mailie/admin.env
sudoedit /etc/mailie/admin.env     # MAIL_ADMIN_KEY=<the key>, once you have one (below)
```

A helper for your shell:

```sh
mailie-admin() {
  sudo systemd-run --quiet --pty --wait --collect \
    --uid=mailie --gid=mailie -p UMask=0077 \
    -p EnvironmentFile=/etc/mailie/mailie.env -p EnvironmentFile=-/etc/mailie/admin.env \
    -E MAIL_DATA_DIR=/var/lib/mailie -E MAIL_HTTP_ADDR=127.0.0.1:8080 \
    /usr/local/bin/mailserver "$@"
}
```

The commands that open the database (`--bootstrap`, `migrate`, `rewrap-credentials`) need the
daemon stopped (`systemctl stop mailie`); the ones that talk to it (`account`, `user`,
`workspace`, `member`, `access`, and `apikey create|list|revoke`) need it running and the admin key
in `/etc/mailie/admin.env`. The `apikey` ones also need `MAIL_ADMIN_API=true` in
`/etc/mailie/mailie.env` (off by default); without it, `apikey create --bootstrap` issues a key
with the daemon stopped. With `--pty` a command can ask for a password at your terminal
(`account add --password -`); to pipe its input instead, use `--pipe`.

## TLS through a reverse proxy

The daemon speaks plain HTTP. **Never expose port 8080:** put a reverse proxy on the same machine
in front of it, and publish only 443 (and 80, for the redirect and certificate issuance). Tools on
other machines then reach REST at `https://mail.example.org`.

What the proxy must do:

- **Pass streams through.** `/v1/events` (Server-Sent Events) streams; a buffered response
  arrives late or never. Turn response buffering off, and request buffering too, so that a message
  or an attachment on its way through is never written to the proxy's temporary files.
- **Allow large bodies.** A send with attachments is up to about 40 MiB.
- **Keep `/metrics` to itself.** The daemon serves Prometheus metrics on its main listener unless
  `MAIL_METRICS_ADDR` gives them one of their own. Answer `/metrics` with a 404 at the proxy.
- **Set `X-Forwarded-For`**, and tell the daemon whose header to believe with
  `MAIL_TRUSTED_PROXIES`. It reads the header right to left, so a client cannot choose its own
  address.

### Caddy

Caddy obtains and renews the certificate itself:

```caddyfile
mail.example.org {
	respond /metrics 404
	reverse_proxy 127.0.0.1:8080 {
		flush_interval -1
	}
}
```

### nginx

```nginx
server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name mail.example.org;

    ssl_certificate     /etc/letsencrypt/live/mail.example.org/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/mail.example.org/privkey.pem;

    # A send with attachments is up to about 40 MiB.
    client_max_body_size 48m;

    location = /metrics { return 404; }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Connection "";
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        # Streams, messages and attachments pass through as they come, and
        # never into nginx's temporary files.
        proxy_buffering off;
        proxy_request_buffering off;
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
    }
}

server {
    listen 80;
    listen [::]:80;
    server_name mail.example.org;
    return 301 https://$host$request_uri;
}
```

### The proxy's address

`MAIL_TRUSTED_PROXIES` is the address the daemon sees the proxy's connections come from:

- **A proxy on the same machine as a binary install:** `127.0.0.1`.
- **A proxy on the host in front of the container:** `172.30.255.0/29`, the stack's own network.
  Docker forwards the published port from that network's gateway (`172.30.255.1`), and
  `compose.yaml` fixes the subnet, so the address stays the same when the network is made again
  (`docker compose down`, then `up`); only this stack's containers are on it. If `docker compose
  up` says the subnet overlaps a network the host already has, choose another in `compose.yaml`
  and set the same here.

Docker publishes ports past host firewalls such as ufw and firewalld: that is why
`compose.yaml` publishes on `127.0.0.1` only. Keep it that way.

## The first key

The command line is a REST client of the daemon, and it authenticates with an instance key of the
`admin` scope. The first one is issued straight into the database, with the daemon stopped (the
Compose steps above already made it):

```sh
# Compose, from deploy/:
docker compose stop mailie
docker compose run --rm mailie apikey create --bootstrap --scope admin --name cli
docker compose start mailie
export MAIL_ADMIN_KEY=<the key it printed>

# systemd, with the helper above:
sudo systemctl stop mailie
mailie-admin apikey create --bootstrap --scope admin --name cli
sudo systemctl start mailie
sudoedit /etc/mailie/admin.env     # MAIL_ADMIN_KEY=<the key it printed>
```

Instance keys reach the operator workspace's mailboxes, which are the ones the command line adds,
and nothing else; they last a year at most (`--days N` for less). Give each tool a key of its own,
with the scope it needs and, with `--accounts IDS`, only some of the mailboxes:
`mailserver apikey create --scope read --name assistant`, through the daemon with
`MAIL_ADMIN_API=true`, or `--bootstrap` with it stopped.

## Mailboxes and OAuth clients

With the key, a mailbox is added, its sync turned on and checked:

```sh
mailie-admin account add --email you@icloud.com --password -        # an iCloud or IMAP password
mailie-admin account add --email you@example.org --paste           # Gmail or Microsoft, on a server
mailie-admin account sync <account id> on
mailie-admin account sync <account id> status
```

(With Compose, `docker compose exec -e MAIL_ADMIN_KEY mailie mailserver …` in place of
`mailie-admin`.) Nothing of a mailbox is stored until its sync is turned on, and `account add` says
so; its first pass reaches 90 days back (`--initial-days N` reaches further, a negative number
everything). The [README](../README.md#the-operators-mailboxes) lists the rest: renaming, pausing,
removing and recovering.

iCloud and other IMAP mailboxes connect with a password, which `--password -` reads at a prompt
that does not show it, or from a pipe: never as an argument, which `sudo`, `systemd-run` and shell
history record. Gmail and Microsoft mailboxes need OAuth clients you register yourself, for your
server: the [README](../README.md#mailboxes-and-providers) walks through both providers.

- **The installed clients are the only way to link Gmail and Microsoft** on a self-hosted server
  (`MAIL_GOOGLE_CLIENT_ID` and `_SECRET`, `MAIL_MICROSOFT_CLIENT_ID`). For each authorization the
  daemon opens a listener on its own loopback, where the provider sends the browser back: a
  browser on the daemon's machine reaches it, and one on another machine, or outside the
  container, does not. On a server, `account add --paste` (and `account authorize ID --paste` when
  a grant has to be renewed) prints the provider's address to open in any browser and takes back
  the address the browser ends on, which looks like a connection error.
- **Google's mail scope is restricted.** An app left unverified shows Google's "unverified app"
  screen and serves at most 100 users over the project's life; in *Testing*, every refresh token
  expires after 7 days, so set the publishing status to **In production**.
- **Microsoft's is a public client:** a "Mobile and desktop applications" platform with the
  redirect `http://localhost`, public client flows allowed and no secret. Entra's security defaults
  block the device-code flow (`--device`), so `--paste` is the way on a server.
- **The web clients** (`MAIL_GOOGLE_WEB_CLIENT_*`, `MAIL_MICROSOFT_WEB_CLIENT_*`, with
  `MAIL_PUBLIC_URL`) are for an edition that serves a web page, whose redirect is a page of its
  own. A self-hosted server has none, but a mailbox the open console of an earlier release linked
  through one refreshes only with it: keep them until such mailboxes are linked again
  ([Moving from the open console](../README.md#moving-from-the-open-console)).

## No MCP

The daemon serves no MCP of its own: `/mcp` answers the API's JSON 404, and the command line has no
`mcp` command and `serve` no `--mcp-stdio`. A tool uses REST with an instance key made for it
([above](#the-first-key)), through the same proxy. MCP is to come through the hosted service, for a
server connected to it. [`docs/mcp.md`](mcp.md) says what went, the warning a retired `MAIL_MCP_*`
variable brings at start, and what to remove from a client configured for an earlier release.

## Health, metrics and logs

- **Health.** `GET /v1/healthz` needs no key and answers `{"status":"ok","version":"…","uptime_seconds":…}`
  once the daemon serves. Check it from the host (`curl -fsS http://127.0.0.1:8080/v1/healthz`),
  through the proxy, or from your monitoring.
- **Metrics.** Prometheus at `/metrics`, on the main listener or on `MAIL_METRICS_ADDR`. Keep it
  off the public proxy.
- **Logs** go to standard output and error: `journalctl -u mailie` or `docker compose logs`. They
  carry ids, never subjects, addresses, bodies, tokens or passwords. `MAIL_ENV=prod` writes them as
  JSON.

## Backups

`mailserver backup` takes a consistent snapshot beside the running daemon, encrypts it under an AWS
KMS data key and uploads it to S3, designed so that the server can create backups but never read
or delete them. [`docs/backup.md`](backup.md) says what it needs in AWS and how to restore.

- **systemd:** [`deploy/mailie-backup.service`](../deploy/mailie-backup.service) and
  [`deploy/mailie-backup.timer`](../deploy/mailie-backup.timer) run it daily as `mailie`, with an
  environment file of its own (`/etc/mailie/backup.env`: `MAIL_BACKUP_BUCKET`,
  `MAIL_BACKUP_KMS_KEY_ARN`, `MAIL_BACKUP_REGION`, and `MAIL_ENV`) that holds none of the daemon's
  secrets. Start the service once by hand and read its log before enabling the timer.
- **Compose:** `docker compose exec mailie mailserver backup`, with the `MAIL_BACKUP_*` variables in
  `deploy/.env`, from the host's scheduler. The snapshot is written to the container's `/tmp`,
  which `compose.yaml` keeps in memory: it needs room for a copy of the database. AWS credentials
  come from the SDK's default chain; on EC2, a container reaches the instance's role only when the
  instance metadata's hop limit is 2 or more.

Without AWS, stop the daemon and copy the data directory (`mail.db`, and `mail.db-wal` and
`mail.db-shm` if they exist), as for an upgrade below; a copy of a running daemon's files can be
inconsistent. Either way, a backup holds the index, the keys' hashes, any people an earlier
release's console made, and the credentials encrypted under `MAIL_CREDENTIAL_KEY_HEX`, which is not
in it: keep the key apart, and the copy as private as the
data directory. Under a KMS key ([below](#credentials-under-aws-kms)) the credentials of a backup
open only where that key's policy lets the daemon decrypt, with the same `MAIL_ENV`.

## Credentials under AWS KMS

Optional, for a server on an EC2 instance. By default the credentials and the send-hash root are
sealed under `MAIL_CREDENTIAL_KEY_HEX`, a key in the server's environment. With
`MAIL_CREDENTIAL_KMS_KEY_ARN` each is sealed instead under a data key of its own, which an AWS KMS
key wraps (a `THCSEAL` envelope), and the key never leaves KMS: the database, its backups and the
server's environment open nothing without a call to KMS that the key policy allows and CloudTrail
records. Every call carries the encryption context `{service: mailie, env: <MAIL_ENV>, purpose,
ref}`: the purpose is `credential/oauth-token`, `credential/password`, `send/hash-root` or
`auth/kdf-salt-key` (the salt key, the key of the salts passwords are derived under, which a server
with no people never makes, and `rewrap-credentials` re-seals where a database from an earlier
release holds one), and the ref is the mailbox's id (random, never derived from an address) or the
meta row of the root or of the salt key. An envelope opens only for its own context, so one moved to another mailbox, field or env
does not. A fifth purpose, `extension/secret`, is only for a binary that embeds the daemon with an
extension that keeps a secret of its own outside the database: `mailserver serve` never asks KMS
for it, and the policy below need not allow it.

- **The key.** A symmetric KMS key (`SYMMETRIC_DEFAULT`, `ENCRYPT_DECRYPT`). Give the daemon its
  full ARN, `arn:aws:kms:<region>:<account>:key/<id>`, never an alias, which whoever may update
  aliases could point at another key; the daemon talks to KMS in the key's region.
- **`MAIL_ENV` set.** It is part of every envelope's context, so the daemon refuses a KMS key
  without it (`prod` on a server), and an envelope sealed under one env does not open under the
  other.
- **The instance's role, and nothing else.** The daemon reaches KMS with the EC2 instance role's
  credentials, through IMDSv2 only: never `AWS_*` variables, `~/.aws` files, profiles, endpoint
  overrides or proxies. `mailserver backup`, which uses the SDK's default chain, is the other way
  round. In a container the instance metadata's hop limit must be 2 or more. At start the daemon
  calls `DescribeKey`, and stops, naming the key and the cause, unless KMS shows that exact ARN,
  enabled and symmetric; so do `rewrap-credentials` and `user disable|delete --bootstrap`, which
  run on the same instance.
- **The key policy, in brief.** Let the instance role call `kms:DescribeKey`, and
  `kms:GenerateDataKey` and `kms:Decrypt` only under Mailie's context; nothing else (no `Encrypt`,
  `ReEncrypt*` or grants), and nobody else decrypts:

  ```json
  {
    "Sid": "TheServerSealsAndOpensItsCredentials",
    "Effect": "Allow",
    "Principal": { "AWS": "arn:aws:iam::<account>:role/<the instance's role>" },
    "Action": ["kms:GenerateDataKey", "kms:Decrypt"],
    "Resource": "*",
    "Condition": {
      "StringEquals": {
        "kms:EncryptionContext:service": "mailie",
        "kms:EncryptionContext:env": "prod",
        "kms:EncryptionContext:purpose": [
          "credential/oauth-token", "credential/password", "send/hash-root", "auth/kdf-salt-key"
        ]
      },
      "Null": { "kms:EncryptionContext:ref": "false" }
    }
  }
  ```

  Give the credentials a key of their own, not the backups' key, on which the server must never be
  allowed to decrypt ([`backup.md`](backup.md)). A server already under KMS adds
  `auth/kdf-salt-key` to this condition **before** it upgrades to the release that brings the key
  scheme ([below](#passwords-the-server-never-receives-migration-0013)): that release's first start
  seals the salt key under it, and stops, refused by KMS, while the policy does not allow it.

A KMS error that says nothing about an envelope (KMS not reached, throttling, a denied call, a
disabled key) stops the start too, but it is never taken for a lost key: nothing tells you to
replace the send-hash root, which opens again once KMS answers. Only an envelope KMS refuses to
unwrap for its context, or one of another key provider, is one the configured keys do not open.

To move a server to KMS, keep the hex key until every row is re-sealed:

1. Create the key and its policy, and add `MAIL_CREDENTIAL_KMS_KEY_ARN` (and `MAIL_ENV`, at the
   value the server will keep: it is part of every envelope's context) to the environment, keeping
   `MAIL_CREDENTIAL_KEY_HEX`, `MAIL_CREDENTIAL_KEY_ID` and `MAIL_CREDENTIAL_PREVIOUS_KEYS` as they
   are.
2. Restart the daemon. Its start log says `credential_sealer=aws-kms`: it seals under KMS from now
   on, and still opens with the hex key what it sealed before. **From this start on, rows move
   under KMS without a rewrap**: every token the daemon refreshes (a Gmail mailbox's within the
   hour) and every mailbox linked or authorized again is sealed under the KMS key, which alone
   opens it. Removing `MAIL_CREDENTIAL_KMS_KEY_ARN` is no longer the way back; the way back is
   [below](#going-back-to-the-hex-key).
3. Stop it, and run `mailserver rewrap-credentials`. It re-seals every credential and the send-hash
   root under KMS in one transaction (each credential's `keyid` becomes 0, which no hex key has),
   or changes nothing if a row does not open; run again, it finds nothing to do.
4. Remove `MAIL_CREDENTIAL_KEY_HEX`, `MAIL_CREDENTIAL_KEY_ID` and `MAIL_CREDENTIAL_PREVIOUS_KEYS`
   from the environment, and start the daemon.

```sh
# systemd, with the helper above:
sudo systemctl restart mailie        # after step 1
sudo systemctl stop mailie
mailie-admin rewrap-credentials
sudoedit /etc/mailie/mailie.env      # remove the three MAIL_CREDENTIAL_KEY* variables
sudo systemctl start mailie

# Compose, from deploy/ (the instance's hop limit at 2 or more):
docker compose up -d                 # after step 1
docker compose stop mailie
docker compose run --rm mailie rewrap-credentials
docker compose up -d                 # after removing the three variables from deploy/.env
```

Keep the hex key elsewhere for as long as a backup taken before the move is kept: a backup's
credentials stay sealed as they were on its day, and putting one into service needs that key in
the environment again, beside the KMS key, until `rewrap-credentials` has run on it.

KMS's own rotation of the key keeps its ARN and opens every envelope it ever wrapped: it needs
nothing here. Moving to another KMS key, or to another `MAIL_ENV`, is not a rewrap: an envelope's
header names neither, so the daemon, given the new one, does not open what the old one sealed, and
no key given beside it can. It stops at start and says so, naming what sealed the send-hash root:
put `MAIL_CREDENTIAL_KMS_KEY_ARN` and `MAIL_ENV` back to those values, and everything opens again.
`rewrap-credentials` refuses too, rather than report everything as already sealed, and
`rewrap-credentials --new-send-hash-root --new-salt-key` does not replace such a root and salt key
(which would forget every send record of a root that still opens) unless `--kms-key-lost` says
that the key that sealed them is lost for good; every mailbox then has to be authorized again, and
each person moves to their new salt at their next sign-in. The one command replaces both, which
the same key sealed; it refuses to replace a salt key that still opens, and is then run without
`--new-salt-key`.

### Going back to the hex key

Without `MAIL_CREDENTIAL_KMS_KEY_ARN` the daemon opens no `THCSEAL` envelope, so leaving KMS takes a
rewrap too, while KMS still answers:

1. Set `MAIL_CREDENTIAL_SEALER=keyring` beside `MAIL_CREDENTIAL_KMS_KEY_ARN` and `MAIL_ENV`, with
   `MAIL_CREDENTIAL_KEY_HEX` (and `MAIL_CREDENTIAL_KEY_ID`) given: the hex key that sealed before
   the move, or a new one. The start log says `credential_sealer=keyring,opens:aws-kms`: the hex
   key seals, and the KMS key, still checked at start, only opens what it sealed.
2. Stop the daemon, and run `mailserver rewrap-credentials`. It re-seals every row the KMS key
   sealed under the hex key, in one transaction.
3. Remove `MAIL_CREDENTIAL_KMS_KEY_ARN` and `MAIL_CREDENTIAL_SEALER`, and start the daemon.

Keep the KMS key, and the policy that lets a host decrypt with it, for as long as a backup taken
while it sealed is kept.

## Upgrades

Each entry below says what its release changed, in that release's words and with its commands; the
newest is last. What a later release took away is in the entry of the release that took it: the
web console in [The open console leaves](#the-open-console-leaves), and people, passwords,
invitations and MCP in [No people and no MCP of its own](#no-people-and-no-mcp-of-its-own). On a
server past those, an older entry's step that signs someone in, invites or resets someone, or uses
a console or an MCP client applies to nobody: the people a database still holds are moved as
[Moving from the open console](../README.md#moving-from-the-open-console) says.

The daemon applies pending schema migrations when it starts (`mailserver migrate --dry-run` lists
them). An older binary does not know a newer schema, so **copy the data directory before every
upgrade**, with the daemon stopped, and keep the version you are leaving: going back is that copy
and that version, together, never the binary alone. Since the release that added workspaces
(migrations 0008 and 0009) a binary refuses to open a database a newer one migrated, and says which
schema each knows; binaries from before it have no such check, and one started on a migrated
database runs on it, creates people without a personal workspace and cannot link a mailbox for
anyone. Should that have happened, the next start of a current binary gives those people their
personal workspace and logs a warning.

That release also changes who becomes the server's first owner: the invite decides
(`user invite --bootstrap --role owner`), never the order of sign-ups. A server upgraded with
invites waiting and nobody signed up yet gets its oldest invite turned into an owner's, the one the
quick start of earlier releases had you make for yourself. A daemon of that release that logged at
start that nobody administered the server wanted an owner's invite, for an address with no account
yet; a server with no people says nothing of owners.

### Team mailboxes belong to their team (migration 0011)

The release that makes a team's mailbox the team's ([`workspaces.md`](workspaces.md)) changes what
a team's people can do, the API and the open console's texts. **Back up first**, as above: going
back is that copy.

- **Sync.** A team mailbox syncs under the team's consent, which an owner or an admin gives in the
  console, to the current sync text, and any of them turns off, deleting its index for everyone. The
  upgrade copies the consent of the person who linked each team mailbox, when that person is active
  and had agreed, so the mailbox keeps syncing; until an owner or an admin confirms it (turns it on
  again at the current text, in the Sync section of the mailbox's details in the console), that
  person turning their own sync off, or being disabled or deleted, stops it and deletes its index,
  as the text they agreed to said. Disabling or deleting that person is refused without `--force`
  while someone else reads the mailbox, and names it; with `--force`, the answer lists the
  mailboxes it stopped (`team_syncs_stopped`). A team mailbox whose linker is **disabled** stays
  stopped with its index, as it was, still tied to that person: deleting them deletes that index.
  The daemon lists them at start (`team mailboxes are stopped with their index kept`): turn each on
  again for its team, or off, or remove it. One that only the disabled linker read is listed with
  the mailboxes nobody can read instead: it can never be read again nor turned on, only removed (or
  turned off, which deletes its index). A person turning their own sync off now touches only their
  personal mailboxes, and those still tied to them.
- **Texts.** The open console's sync text is now `2026-10-open-sync-3` and its actions text
  `2026-10-open-actions-2`: everyone is asked again, actions are refused until they agree, and sync
  keeps running for whoever agreed before. A server that sets `MAIL_CONSENT_VERSION_SYNC` or
  `MAIL_CONSENT_VERSION_ACTIONS` keeps its own.
- **Roles.** Owners and admins manage every mailbox of their team by their role (its card,
  re-authorizing it, who holds what) and read none of them by being one. They give `act` and `send`
  to anyone in the team (`act` to someone who reads), and `read` only while they read the mailbox
  themselves. A member sees only the mailboxes they hold something on, no longer lists the team's
  members or who holds what, and no longer leaves by themselves (neither does an admin): an owner or
  an admin removes them. A stored `manage` goes from owners and admins, whose role gives it, and
  stays for members, for whom it means the card and re-authorizing.
- **Protections.** The last person who can read a team mailbox keeps `read`, and is not disabled,
  removed, or closed (`user disable|delete`) without `--force`, unless they are the team's only
  member and it goes with them; a team whose other members are all disabled outlives them, so it
  counts. Nobody else is protected for having linked a mailbox, and taking a link over is gone. A
  mailbox left with no reader (a forced closure) syncs nothing more, cannot be turned on, is marked
  for its team's owners and admins, and is listed at start: remove it, or remove it and link it
  again.
- **Invites** waiting whose creator could no longer make them (deleted, disabled, no longer an
  instance owner, no longer an owner or an admin of the team allowed that role) are expired. From
  now on an invite expires when its creator's role or status in the team changes, when they leave
  it, and when they are disabled or deleted.
- **API changes** for scripts: `POST /v1/accounts/{id}/take-over` is gone; `DELETE
  /v1/accounts/{id}` needs `?confirm=<the same id>` (`mailserver account remove` sends it);
  `linked_by` is gone from accounts, and is attribution only in the access directory, which also
  says each mailbox's readers and its team's consent; a member's `links` became `last_reader_of`;
  `GET /v1/workspaces/{id}/members` and `/access` answer `403` to members; `PUT
  /v1/accounts/{id}/sync` also takes a team mailbox, from its owners and admins, with the sync
  text's `version`, and answers `409` for one nobody can read; `POST /v1/accounts` into a team takes
  `sync_consent_version`. `GET /v1/accounts` (and MCP's `list_accounts`) now list every team mailbox
  to its team's owners and admins, including those they do not read, with `access.read` false:
  check `access.read` before searching or reading one, or it answers `403`. `POST
  /v1/users/disable|delete` add `team_syncs_stopped` to their answer.

### API keys belong to their workspace (migration 0012)

The release that makes every API key its workspace's ([`workspaces.md`](workspaces.md#api-keys))
changes who creates keys and what an existing one reaches. **Back up first**, as above.

- **Who creates keys.** Only an owner or an admin of a workspace, signed in to the console, creates
  its keys, lists them (every key of the workspace, whoever created it) and revokes them; in a
  personal workspace that is its person. **A member of a team no longer creates keys**: an owner or
  an admin creates one for them, holding what they decide. `POST /v1/me/apikeys` is gone: it answers
  `400` and says keys are created in a workspace (`POST /v1/workspaces/{id}/apikeys`).
  `GET /v1/me/apikeys` lists the keys a person created, in every workspace, and
  `DELETE /v1/me/apikeys/{prefix}` still revokes one of them.
- **What a key reaches.** A key acts as nobody now: it reaches exactly the mailboxes it holds
  (`read`, `act`, `send`), given by an owner or an admin — `read` only by one who reads the mailbox —
  and keeps them whoever gave them, until an owner or an admin takes them out or revokes it. It
  stops when the person who created it leaves the workspace or is disabled or deleted; demoting
  them keeps it.
- **Existing keys are carried over**, each with what it reached at the upgrade and nothing more:
  a key made for chosen mailboxes keeps those its person still reads; **a key made for every mailbox
  of its person now lists the mailboxes its person read at the upgrade, and gets none linked later**
  — an owner or an admin who reads a new mailbox adds it to the key. A key is `write` where it acted
  before (its person held `act`), and sends nothing. It goes into the workspace those mailboxes are
  in. A key whose mailboxes spanned several workspaces is **carried over** frozen: it keeps exactly
  those, gains nothing, expires within a year at most, is revoked when its last mailbox goes, and
  each workspace it reaches lists it; the daemon says at start how many are live. A team may hold
  more than 20 live keys this way: those moved in do not count toward its limit of 20, and expire
  on their own. A key nobody agreed to the key terms through — one an administrator made for a
  person, refused everywhere since keys had terms — is revoked, holding nothing, and stays listed
  in its person's personal workspace only, going with it when that person is deleted. Instance
  keys are unchanged.
- **Keys may send.** A key with the `send` scope sends from the mailboxes it holds `send` on, every
  send with `confirm: true`, at most 100 a day, under the address alone. `MAIL_KEYS_MAY_SEND=false`
  turns that off for every workspace key, for a console whose key terms do not cover sending. **A
  server that sets `MAIL_CONSENT_VERSION_KEYS` to its own terms must now set `MAIL_KEYS_MAY_SEND`
  and `MAIL_KEYS_ACT_UNDER_CREATOR_CONSENT` too** (`true` or `false`, as those terms say), or the
  daemon refuses to start. A send without an
  `Idempotency-Key` is now keyed by the message, the key and the minute: another key sending the
  same message from the same mailbox in the same minute sends it, where it was refused before.
- **Texts.** The open console's key terms are now `2026-10-open-api-keys-2`: a new key needs them;
  keys created under the earlier text keep working under it, never send, and, as that text said,
  act only while the person who created them allows actions. Its actions text is now
  `2026-10-open-actions-3`, which says that a key created from now on acts under the key terms,
  not under anyone's choice about actions: turning actions off no longer stops it, taking Act away
  from it or revoking it does. Everyone is asked about actions again, and their actions are refused
  until they agree. A server that sets `MAIL_CONSENT_VERSION_KEYS` or
  `MAIL_CONSENT_VERSION_ACTIONS` keeps its own.
- **API changes** for scripts: the key routes are `/v1/workspaces/{id}/apikeys` (list, create,
  revoke, `…/accounts/{account_id}` to give or take a mailbox, `…/sends`); the access directory
  lists each mailbox's keys; `/v1/apikeys` (the operator's) lists every key with its
  `workspace_id`, and `mailserver apikey list` shows it. A key reading its own consent
  (`GET /v1/me/*-consent`) is `403`: it has no person. `send.finished` names a key's send with
  `sent_by`.

### The send-hash root is kept in the database

The release that keeps the send-hash root in the database needs nothing configured. A send record
keeps a keyed hash of the message it sent, which recognises a send repeated with the same
`Idempotency-Key`, and a send without one is keyed by that hash and the minute. The key of those
hashes used to be derived from `MAIL_CREDENTIAL_KEY_HEX`. It is now a random root that the first
start of the new version makes and keeps in the database, sealed under the credential key as a
credential is, so a rotation of the key no longer changes it.

- **The root changes once, at the upgrade.** A send record made before it is no longer recognised:
  a send without an idempotency key that a client repeats across the upgrade, within the minute,
  is sent again rather than answered from its record, and one repeated with the same
  `Idempotency-Key` is refused as that key reused for another message (`409`) rather than
  replayed. Upgrade when nothing is sending. Nothing else changes, the credentials included.
- **The daemon refuses to start without the key that sealed it.** A daemon whose environment lacks
  that key (a previous key taken out of `MAIL_CREDENTIAL_PREVIOUS_KEYS` before
  `rewrap-credentials` ran, a restored backup without its key, a mistyped key) says so at start,
  where it used to start and fail on each mailbox. Give it the key. If that key is lost for good,
  `mailserver rewrap-credentials --new-send-hash-root`, with the daemon stopped, puts a new root in
  place and touches nothing else; every mailbox then has to be authorized again, as before. It
  replaces only a root no configured key opens, and refuses one that opens. From the release that
  brings the key scheme on, the salt key is sealed beside the root, and a lost key takes both:
  `--new-send-hash-root --new-salt-key` replaces them in one run
  ([below](#passwords-the-server-never-receives-migration-0013)).
- **`rewrap-credentials` re-seals the root** with the credentials, under the active key.

### Passwords the server never receives (migration 0013)

The release that brings the key scheme ([`key-scheme.md`](key-scheme.md)) changes how everyone
signs in. **Back up first**, as above: going back is that copy, and the version you are leaving.

- **Nobody's password reaches the server any more,** but once. Each person's browser derives an
  auth key from the password, which the server keeps as a hash, and keeps an account key, which the
  browser makes, under the password and under a recovery code shown once. People who signed up
  before keep signing in with the same password: at their first sign-in on the new version the
  console sends it in clear one last time, the server checks it against the old hash, and the
  browser enrols them (it shows them their recovery code, which they should keep). From then on the
  server refuses their password in clear, as a wrong one.
- **The old way in exists in this release only.** The route that takes the password in clear for
  the upgrade (`POST /v1/auth/upgrade/login`), and the challenge's `upgrade` answer that sends a
  browser there, leave in the next release. Until then a challenge tells anyone who asks that an
  address has an account not yet upgraded (the key scheme's threat model, section 5.5). Ask
  everyone to sign in once on this version; whoever has not by the next one gets a reset
  invitation from you (below). The next release removes it
  ([The upgrade leaves](#the-upgrade-leaves-the-release-after-the-key-schemes)): a server that goes
  straight from a release before the key scheme to a later one has no upgrade at all.
- **New passwords have at least twelve characters**; one chosen before keeps working, however
  short.
- **Nothing needs configuring, but on a server under AWS KMS.** The first start makes a **salt
  key**, the key of the salt every address's password is derived under, and keeps it in the
  database sealed under the credential key as the send-hash root is (`meta.kdf_salt_key`). Under
  a KMS key ([above](#credentials-under-aws-kms)) it is sealed for the purpose
  `auth/kdf-salt-key`: add that purpose to the key policy's condition **before** you upgrade, or
  the new version stops at its first start, refused by KMS. The daemon refuses to start without the
  key that sealed it, as for the root; `rewrap-credentials` re-seals it with the credentials, and
  if its key is lost for good, `mailserver rewrap-credentials --new-salt-key`, with the daemon
  stopped, puts a new one in place: everyone keeps signing in, and moves to their new salt at
  their next sign-in. A key lost for good (a backup restored without it, a KMS key deleted) has
  sealed the send-hash root too: `mailserver rewrap-credentials --new-send-hash-root
  --new-salt-key` (with `--kms-key-lost` for a KMS key) replaces both in one run, and the daemon
  starts. A daemon whose people sign in only through an extension makes none.
- **A forgotten password** is the recovery code's to replace, in the console. A person who lost
  both now gets a **reset invitation** rather than a password set for them: see
  [No people, and no passwords](#no-people-and-no-passwords).
- **Sessions open at the upgrade stay open,** with no step-up time. The person enrols at their next
  sign-in; until then the console asks them to sign in again for what needs their account key
  (changing the password or the recovery code, each of which asks for the password again). The
  release that brings mailbox keys (migration 0014, below) asks for a step-up when a key or a grant
  is written. Changing the password ends every session, as before.
- **Going back from this release costs those who signed in on it.** Each browser that enrolled
  someone remembers it, and never sends that person's password in clear again. If you go back
  (below) and later upgrade again, the restored database asks everyone who signed in on this
  version for the upgrade once more, and their browsers refuse, telling them to ask you: give each
  a reset invitation ([No people, and no passwords](#no-people-and-no-passwords)), or have them clear the site's
  data in their browser. Restoring a backup from before this version onto it does the same.
  Upgrading again to a later release, which has no upgrade, each of them needs a reset invitation.
- **The API.** `POST /v1/auth/login` takes `{email, auth_key}`; `POST /v1/auth/signup` an
  enrolment; `POST /v1/auth/password` is gone, replaced by `/v1/auth/password/begin` and
  `/finish`; new routes answer the challenge, recovery, step-up, the reset invitation and the
  upgrade ([`console.md`](console.md#routes)). A script that signed in with a password needs an
  API key instead, as it always should have.

### Mailbox keys (migration 0014)

The release that brings mailbox keys ([`key-scheme.md`](key-scheme.md) sections 8, 9 and 12.11 to
12.15) adds two tables and changes no row. **Back up first**, as above: going back is that copy.

- **Every mailbox you have stays without a key**, and is read as before, by the "read" flag. The
  first time someone who reads one signs in to the console on this version (after enrolling, at
  the upgrade of 0013 or since), their browser writes its key right after that sign-in, sealed to
  everyone else who reads it and has enrolled, while the sign-in counts as a step-up; a session
  only reopened from before writes none, and the console offers to write it on the mailbox, behind
  a step-up. Mailboxes linked from now on get their key with the link. Operator mailboxes, linked
  from the command line, keep none.
- **Members who wait for the key.** Once a mailbox has a key, reading it takes the flag and the
  person's grant. Someone who holds "read" but had not enrolled when the key was written (they have
  not signed in since the upgrade of 0013) sees the mailbox as waiting for its key, and reads
  nothing of it, until someone who reads it hands them the key in the console after they enrol.
  The same goes for a person you reset (`user password --bootstrap`), on every mailbox that has a
  key: a reader of a team's mailbox gives them the key again, and their own mailboxes they key
  again themselves.
- **A step-up for keys and grants.** Linking a mailbox, giving "read" with a grant (on a mailbox
  that has a key, to someone who has enrolled), handing on a mailbox's key and writing a mailbox's
  key need the person's password within the last ten minutes (a sign-in counts); the console asks
  for it again when it is older. "Read" by the flag alone, on a mailbox without a key or to someone
  who has not enrolled, needs none, nor does any other change of flags.
- **A script that links a mailbox with an API key** links an operator mailbox, as before, which has
  no key; a person's console sends the key with the link, and a server of this release refuses a
  person's link without one.
- **Going back is the backup.** An older binary refuses schema 14; restoring the copy taken before
  the upgrade, with the version you kept, loses every mailbox key and grant written since: the
  mailboxes are read by the flag again, and the console writes their keys again when you upgrade
  again.

### Account keys of people who sign in through an identity provider (migration 0015)

The release that brings migration 0015 adds two empty tables and changes no row. They hold the
account key of people who sign in through an identity provider, which only a binary that embeds the
daemon with an extension brings ([`console.md`](console.md#signing-in-through-an-extension)):
`mailserver serve` has none, and leaves them empty. **Back up first**, as above: an older binary
refuses schema 15. Where an extension signs people in, a person without an account key is given no
session any more: their next sign-in through the provider makes one
([`key-scheme.md`](key-scheme.md) section 12.10).

### The upgrade leaves (the release after the key scheme's)

The release after the one that brings the key scheme removes the upgrade of accounts made before it
([`key-scheme.md`](key-scheme.md) section 12.7), the one time the console sent a password in
clear. No migration runs and the schema does not change; **back up first**, as above, all the same.

- **Accounts that never signed in during the release that brought the key scheme sign in no
  more.** A person who signed up before the key scheme and did not sign in on that release has not
  enrolled: the console sends their old password nowhere, and a sign-in of theirs fails as a wrong
  password does. That release's way back for each was a **reset invitation**, printed with the
  daemon stopped (`mailserver user password --bootstrap --email ADDRESS`, a command a later release
  removed: [No people, and no passwords](#no-people-and-no-passwords)): with it they chose a new
  password and got a recovery code and their account key, and their old password hash was cleared.
  A server that went straight from a release before the key scheme to that one or a later one
  needed one for everyone who had a password.
- **Someone still signed in from before the key scheme** keeps that session until it ends (14 days
  at most from its sign-in); the console tells them, in their account, that their password no
  longer signs them in and to ask you for a reset link. They cannot link a mailbox or change their
  password meanwhile.
- **Nothing else changes for people who enrolled.** Their sign-in, recovery code and keys are as
  they were. Their browsers forget the list of addresses that enrolled there, which only the
  upgrade used.
- **The API.** `POST /v1/auth/upgrade/login` and `/v1/auth/upgrade/enrol` answer `404`, and the
  challenge answers `{salt, kdf}` without `upgrade` ([`console.md`](console.md#routes)). Their old
  password hashes stay in the database, which nothing reads to sign anyone in, until a reset
  clears each one.
- **Going back** to the release that brought the key scheme is the copy and that version, as ever;
  the upgrade then works again there for whoever has not enrolled.

### A workspace's kind, source and person never change (migration 0016)

The release that brings migrations 0016 to 0019 adds three columns and triggers, and changes no
row. **Back up first**, as above: an older binary refuses the migrated schema, and going back is
that copy. Migration 0016 adds a trigger: a workspace's kind (personal, team or the operator's),
its source and the person of a personal workspace never change, whatever writes the row. Nothing
in the server ever changed them; a script that edits the database directly is refused.

- **A team from another source outlives its people.** Only a binary that embeds the daemon with an
  extension has teams whose source is not local; `mailserver serve` has none. Such a team stays,
  with its mailboxes, when its last member here is deleted, and no count of its members here makes
  anyone its last owner ([`workspaces.md`](workspaces.md#the-workspace-source)). A local team is as
  before: one whose only member is deleted goes with them.

### Why an API key stopped (migration 0017)

Migration 0017 adds `api_keys.stop_reason` and changes no row.

- **A key that stops says why**, from now on: `manual` (revoked), `creator_left`,
  `creator_disabled`, `creator_suspended`, `creator_deleted`, `account_recovered` or
  `restriction_emptied` ([`workspaces.md`](workspaces.md#api-keys)). The listings answer it as
  `stop_reason` on a revoked key (`GET /v1/apikeys`, `/v1/workspaces/{id}/apikeys` and
  `/v1/me/apikeys`), `mailserver apikey list` prints it after `revoked`, and the console says it
  beside the key. A key revoked before the upgrade says none: nothing recorded one. The schema
  refuses a key that stops without a reason, and a reason that changes, so a script that revokes
  keys by writing to the database directly must name one.
- **A recovery or a reset now stops the API keys the person created**, in every workspace, saying
  `account_recovered`, where it ended their sessions only: recovering an account with its recovery
  code in the console, and completing a reset invitation
  ([No people, and no passwords](#no-people-and-no-passwords)). Whoever held the person's password, their
  recovery code or a session of theirs may have created a key, and a key outlives a session. The
  console's recovery and reset forms say so before the person confirms, and
  `mailserver user password --bootstrap` when it prints the link, with how many of their keys are
  live then; whoever uses one of those keys in a tool needs a new one, which the person creates once
  signed in. Changing the password, replacing the recovery code and signing out everywhere keep
  them, as before. A key asked for by a session that a recovery or a reset has just ended is
  refused (`401`).
- **Texts.** The open console's key terms are now `2026-10-open-api-keys-3`: they say a key also
  stops when its creator's account is recovered, with the recovery code or a reset link. A new key
  needs them; a key created under `2026-10-open-api-keys-2` keeps working under those terms. A
  server that names its own key terms (`MAIL_CONSENT_VERSION_KEYS`) changes nothing here.

### People suspended by an extension (migration 0018)

Migration 0018 adds `users.suspended_at`, 0 for everyone, and changes no row: a person disabled
before stays disabled here. A person is suspended only by an extension of the daemon, following a
decision taken elsewhere ([`workspaces.md`](workspaces.md#people-on-the-instance)); `mailserver
serve` has none, so nobody is ever suspended on a server it runs, and nothing changes for it. On a
binary that has one, a suspended person is not active, as a disabled one is not: every session,
key and waiting invite of theirs ends, and their grants stay, unusable, for a resume. A disable
here stands over a suspension, and `mailserver user password --bootstrap` says when its person is
suspended. The members listing says `person_suspended` of a suspended member.

### Closed teams (migration 0019)

Migration 0019 adds `workspaces.closed_at`, 0 for every workspace, so every team stays open, and
the triggers that refuse giving a closed team anything.

- **The operator may close a team**, with the daemon running: `mailserver workspace close
  --workspace ID` (or `POST /v1/workspaces/{id}/close` with an unrestricted instance admin key),
  and `mailserver workspace reopen --workspace ID` to undo it. While a team is closed nothing in it
  is used: its mailboxes reach nobody, its owners, its admins and its keys included, and sync
  nothing, keeping their index, their consent to sync, their grants and what the team's keys hold;
  and nothing in it changes, by anyone, the operator included (`409`), until it reopens. Closing
  it expires the invites into it still waiting and ends the consent attempts on its mailboxes.
  Reopened, it is as it was, and its mailboxes catch up. No person closes a team, its owner
  included, and the console has no such action ([`workspaces.md`](workspaces.md#closed-teams)).
- **The daemon says at start** which teams are closed (`teams closed`), and `mailserver workspace
  list` and `GET /v1/workspaces` show each with when it closed (`closed_at`).

### Removed mailboxes are kept 30 days (migration 0020)

Migration 0020 adds four columns to `accounts`, the mailbox journal and people's notices, and
changes no row: every mailbox stays as it is. **Back up first**, as above: an older binary refuses
schema 20, and going back is that copy, in which a mailbox removed since the upgrade is live again
and one deleted for good since is back. What removing a mailbox does changes
([`workspaces.md`](workspaces.md#removed-mailboxes)):

- **Removing keeps it 30 days.** A mailbox removed from now on stays, with its credentials, its
  index, its key and its consent, used by nothing: nobody finds it, it syncs nothing, and it is
  never dialled nor refreshed. Within those days its owners recover it or delete it for good;
  after them the hourly sweep deletes it for good, and so does the daemon's start, so that a
  backup restored late deletes at once what is overdue. What API keys held on it, and its consent
  attempts, go at once and never come back. A link that never connected is deleted at once, as
  before. Its space on disk comes back only when it is deleted for good: when its person asks you
  to erase it now, delete it for good yourself (`account purge`, below).
- **Only owners remove a team's mailbox.** An admin of a team no longer removes its mailboxes, but a
  link of their own that never connected. A team's active owners remove them, see the removed ones
  below its cards, recover them and delete them for good, in the console; a personal mailbox is its
  person's to do all of that, as before. Everyone who read a removed mailbox, but whoever removed
  it, is told with a notice in the console, and so are the other readers of a team mailbox whose
  sync is turned off.
- **A recovered mailbox syncs nothing until it is turned on**: by its person, for a personal
  mailbox; by a team's owners or admins, to the current sync text; with `mailserver account sync
  ID on`, for an operator mailbox. It then catches up from the index it kept, with the readers who
  still qualify and nothing an API key held. A team mailbox nobody who could read it is left to
  read is not recovered: delete it for good and link it again. Nothing is recovered in a closed
  team, whose removed mailboxes are deleted on time all the same.
- **A recovered mailbox may need authorizing again.** Nothing refreshes its token while it is
  kept, and a grant that ended meanwhile shows when its sync is turned on: the mailbox then needs
  authorizing, from its card in the console (`mailserver account authorize ID` for an operator
  mailbox). Under a Google OAuth client left in *Testing*, whose refresh tokens expire 7 days after
  they are issued ([README](../README.md#google)), expect it of every Gmail mailbox recovered more
  than 7 days after it was last authorized; a client **In production** has no such limit.
- **An address kept removed is not linked again.** Linking it in the same workspace is refused
  (`409`), saying until when it is kept: an owner recovers it, or deletes it for good and links it
  again. A script that removed a mailbox and added it again (to replace an iCloud app-specific
  password, which no route replaces) now deletes it for good in between.
- **The command line.** `mailserver account remove` says until when the mailbox is kept, or that one
  that never connected was deleted at once. `account removed [--workspace ID]` lists a workspace's
  removed mailboxes (the operator workspace's by default). `account recover ID` recovers an
  operator mailbox, whose sync `account sync ID on` then switches on again; a person's or a team's
  is their owners' to recover, never yours. `account purge ID [--yes]` deletes a removed mailbox
  of any workspace for good, after asking: how you erase one when its person asks you to.
  `rewrap-credentials` re-seals the credentials of removed mailboxes too. Closing a person
  (`user delete`) deletes at once, for good, the removed mailboxes that go with them, as it does
  their live ones.
- **Texts.** The open console's sync text is now `2026-10-open-sync-4`, which says all this:
  everyone is asked again, and sync keeps running for whoever agreed before. A server that sets
  `MAIL_CONSENT_VERSION_SYNC` keeps its own text; if that text says removing a mailbox deletes its
  index, it no longer does, and the text needs a new revision.
- **API changes** for scripts: `DELETE /v1/accounts/{id}?confirm=<id>` answers `200` with
  `{account_id, purged, purge_after}` where it answered `204`; `GET
  /v1/workspaces/{id}/removed-mailboxes`, `POST /v1/accounts/{id}/recover` and `POST
  /v1/accounts/{id}/purge` (`{"confirm": "<id>"}`) are new; `POST /v1/accounts` answers `409` for an
  address kept removed; `AccountSync` has `held` and `held_since` while a recovery holds a
  mailbox's sync, which `PUT /v1/accounts/{id}/sync` `{"enabled": true}` lifts, from a personal
  mailbox's person too; a person's event stream may say `event: notice` (no `id`), and
  `GET /v1/me/notices` lists their notices ([`console.md`](console.md#routes)). MCP is unchanged:
  no tool lists or reaches a removed mailbox.

### The journal rebuilt, and room for an extension's directory (migrations 0021 to 0023)

Migration 0021 **rebuilds** the mailbox journal (`mailbox_events`), the second migration to rebuild
a table: `mailserver migrate --dry-run` says so. It copies every entry, with who read the mailbox
then and every notice still to read, and adds what an entry may say from now on: a mailbox
*stranded* (left with nobody who reads it by a person's forced disable, deletion or key reset, or by
what an external directory decided), why, and a removal's end. Migrations 0022 and 0023 add tables
and columns that only an extension following an external directory writes, in a binary that embeds
the daemon; `mailserver serve` has none, so they stay empty and every column 0 on a self-hosted
server. No row changes but a journal entry's name, below. **Back up first**, as above: an older
binary refuses schema 23, and going back is that copy, which loses everything written since the
upgrade, as any restore does; the older version needs nothing this release adds.

- **The journal names a mailbox less often.** A display name holding the local part of the
  mailbox's own address (three characters or more, whatever the case: "Support" for
  `support@example.org`) is no longer kept in the journal or shown in a notice, which then names
  the mailbox by its masked address (`s…@example.org`): the rebuild leaves such a name out of the
  entries already there too, and, where the name or the local part has a character beyond plain
  ASCII, leaves the name out rather than compare it.
- **Forced closures are journaled.** Disabling, deleting or resetting a person with `--force`, where
  it leaves a team mailbox with nobody who reads it, records that mailbox as stranded, by whoever
  closed them (`cli` for `mailserver user disable|delete --bootstrap`); nobody is told in the
  console, and deleting the person forgets them there too, as it forgets that they acted.
- **A Google mailbox deleted for good has its grant revoked at Google.** When the sweep deletes a
  removed Gmail mailbox after its 30 days, when its owners or the operator delete it before, and
  when the person it goes with is deleted (over the API or with `mailserver user delete
  --bootstrap`, which needs the same egress), its refresh token is sent to Google's
  revocation endpoint (`https://oauth2.googleapis.com/revoke`) once the deletion has committed, so
  the grant no longer appears among the person's connected apps. It is best effort: a failure is
  logged (without the token) and never stops the deletion; a server whose egress cannot reach that
  address only logs it. The same Google account linked again in another workspace, under any
  spelling of a Gmail address and through either OAuth client, shares one grant there, which is
  then kept until the last such mailbox goes: Google keeps one grant per account and Cloud project,
  and revoking it ends it for every client of the project, the Desktop and the Web one alike.
  Microsoft 365 offers no revocation of a single grant, and a password account has nothing to
  revoke: their stored credentials are deleted, as before.
- **The console** keeps how long the server asked to wait before trying again (`Retry-After`), and,
  when the workspace it shows is no longer the person's under a session still open, reads their
  workspaces again once and shows their own.

### A removed mailbox keeps protecting until it is deleted for good

No migration: the schema stays 23 and no row changes, so going back is the binary (and console)
you kept. What the server refuses changes
([`workspaces.md`](workspaces.md#removed-mailboxes)):

- **The last reader of a removed team mailbox stays protected.** A team mailbox removed and kept
  for its 30 days protected nobody until now: the one person who would read it again once
  recovered could leave the team, be removed or disabled there, or be closed or reset without
  `--force`, and a recovery then brought back a mailbox nobody could read. It now protects them as
  a live mailbox does, until it is deleted for good: removing them from the team or disabling them
  there is refused (`409`), and so are `mailserver user disable|delete` without `--force`, and
  `user password --bootstrap` without `--force` where the removed mailbox has a key (on one
  without, they keep reading by the flag, which a reset does not take), naming the mailbox; with
  `--force` the mailbox is journaled stranded, as a live one is. Closing the linker a migrated team
  consent is still bound to is refused without `--force` too while someone would read the removed
  mailbox once recovered, since it would delete the index the mailbox keeps for that recovery.
  Removing a mailbox no longer frees its last reader: recover it and give someone else read, or
  delete it for good (its owners in the console, or `mailserver account purge ID`), which ends the
  protection with it, as the hourly sweep does after its 30 days.
- **Nothing else of it changes.** It still syncs nothing, nobody reads it, its card is found by
  nobody but its owners on the removed mailboxes' own paths, and nothing is given on it.
- **For scripts:** a team member's `last_reader_of` (`GET /v1/workspaces/{id}/members`, and the
  `NOTE` of `mailserver member list`) may name a removed mailbox, by its id alone, to the team's
  admins as well as its owners; `GET /v1/accounts` does not list it, and only the team's owners
  find it in `GET /v1/workspaces/{id}/removed-mailboxes`. The console names it as removed, and
  says how to free its last reader.

### When a permission was turned off (migration 0024)

An ordinary migration: two columns of `users` that record when a person last turned actions or
sending off (`actions_consent_withdrawn_at`, `send_consent_withdrawn_at`), 0 for everyone at the
upgrade. No other row changes. **Back up first**, as above: an older binary refuses schema 24, and
going back is that copy, which loses everything written since the upgrade, as any restore does.

- **For scripts:** `GET`, `POST` and `DELETE /v1/me/actions-consent` and `/v1/me/send-consent`
  answer `withdrawn_at` (Unix seconds) while the person has turned that permission off and not
  given it again; it is absent for someone who consents, who never answered, or who turned it off
  before the upgrade ([`console.md`](console.md#consent-to-actions)). Nothing else of the answers,
  and nothing of who may act or send, changes.
- **The open console** asks as before: it offers actions again to a person who turned them off.

### A grant proved before it is stored, and the operator's turn-off of any mailbox

No migration: the schema stays 24 and no row changes, so going back is the binary (and console) you
kept.

- **A new grant is stored only once it opens the mailbox.** Connecting a mailbox through Google or
  Microsoft, or authorizing one again, ends with one IMAP login with the new grant. A mail server
  that could not be reached then (down, slow, throttling) used to leave the grant stored unproved;
  now nothing is stored: the attempt answers `500`, "upstream: …", with `Retry-After: 30`, a
  mailbox that never connected shows why (`the mail server could not be reached to prove this
  authorization`), and one authorized again keeps the grant it had. Start again once the mail
  server answers ([`console.md`](console.md#mailboxes)).
- **`mailserver account sync ID off` turns any mailbox's sync off**, of any workspace, removed ones
  and a closed team's too (`POST /v1/accounts/{id}/sync/stop`, an unrestricted admin key): how you
  erase a mailbox's index when asked to, or stop one being abused. Its index is deleted, whoever
  read it is told in the console, and the journal names the operator. A team's owners or admins
  turn it on again, to the current sync text; a personal mailbox is held, its person's consent
  kept, until they turn it on. An operator mailbox's is switched off as before
  ([`workspaces.md`](workspaces.md#consents)).
- **For a binary that embeds the daemon** (nothing changes for `serve`): the freshness hook is also
  asked before a team's owner deletes a removed team mailbox for good or turns its kept index off
  (`purge`, `removed_sync_off`), once the rest of the authorization let the act through (a team
  withheld or closed is refused first), and `app.Options.KeysInPersonalWorkspaceOnly` keeps API
  keys to personal workspaces, a team's mailbox reached by none.

### Pausing one mailbox's sync (migration 0025)

A **rebuild** of the journal (the third migration to rebuild a table, after 0008 and 0021, and the
journal's second; `mailserver migrate --dry-run` says so) and two columns of `accounts` (`sync_paused_at`, `sync_paused_by`). Every row is kept, but for
the holds it converts. **Back up first**, with the commands below: an older binary refuses schema
25, and going back is that copy.

- **A recovered mailbox's hold becomes its pause**, with the hold's moment, where its consent
  stands, and goes where it is off: the mailbox's kept index is read, searched and acted on at
  once, and someone who may resumes it. A personal mailbox the operator turned off stays held, and
  so does a personal hold over no index the journal does not show to be a recovery's (one older
  than 90 days); a removed personal mailbox the operator turned off is held, so that its recovery
  keeps it turned off ([`workspaces.md`](workspaces.md#migration-0025)).
- **A mailbox's sync can be paused**, its index kept, by its person, a team's owners and admins
  (for the team; its other readers are told), or the operator for an operator mailbox, and resumed
  by the same people; a team mailbox's resume agrees again, for the team, to the current sync text
  ([`workspaces.md`](workspaces.md#pausing-a-mailboxs-sync)). A recovery now pauses a mailbox
  whose consent stands, instead of holding it.
- **For scripts:** `POST /v1/accounts/{id}/sync/pause` and `…/sync/resume` (`{"version"}` for a
  team mailbox, the current sync text); `sync.paused` and `paused_since` on every account; `held`
  now means "held until its person turns it on" (the operator's turn-off, or such an older hold);
  `PUT /v1/accounts/{id}/sync {"enabled": true}` no longer brings a recovered mailbox back: resume
  it; `GET /v1/workspaces/{id}/access` answers `paused_at` and `paused_by`; a notice may be of kind
  `paused`; `POST /v1/accounts/{id}/sync` (sync now) answers `409` while paused; a recovered
  mailbox is searchable and acted on at once.
- **The command line:** `mailserver account sync ID pause|resume`, for an operator mailbox;
  `account recover` says whether it came back paused or off.
- **The console** shows one "Paused" state, with Resume (a team's through the sync text), and the
  operator's turn-off as "Turned off". The open sync text does not change.

### Linking Gmail and Microsoft at the provider (migration 0026)

An ordinary migration: one table added, `oauth_links`, and no row of a schema-25 database changes
(`mailserver migrate --dry-run` names it, with no rebuild). An older binary refuses schema 26, so
**back up first**, with the commands below; going back is that copy.

- **The console connects a Gmail or Microsoft mailbox without its address**, where the server
  offers the web flow: the person chooses the account on Google's or Microsoft's page, the daemon
  asks for `openid email` beside the mail scopes, and the provider's ID token names the address,
  which one login then proves before anything is stored. Add the two sign-in scopes to Google's
  **Data access** and to the Microsoft web registration's **API permissions**
  ([README](../README.md#mailboxes-and-providers)); the clients' ids, secrets and redirect URIs do
  not change. A Microsoft tenant whose administrator consented before they were added consents
  again; until then, and for a shared mailbox always, its people use "Enter the mailbox's address
  instead". Re-authorizing a mailbox, the command line, the loopback flow and a browser that
  refuses site storage still take the address, and ask for the mail scopes alone.
- **One Gmail account is one mailbox per workspace**, under every spelling of its address (case,
  dots, a `+tag`, `googlemail.com`), live or removed, on both paths: a second spelling is `409`.
  Mailboxes linked before the rule stay as they are.
- **A Google grant is revoked at a deletion for good only once no other mailbox here holds the
  account**, under any spelling of its address and through either OAuth client: Google keeps one
  grant per account and Cloud project, and revoking it ends it for every client of the project.
- **Connecting at the provider asks for a recent password with time to spare**: the console asks
  again when fewer than five minutes of the ten remain before it leaves for the provider, and two
  at the return.
- **For scripts:** `POST /v1/accounts/oauth/links` (`{provider, workspace_id?,
  sync_consent_version?, display_name?}`, the web flow only) answers `201 {link, auth}`;
  `GET /v1/accounts/oauth/links/{id}` reads it, for its starter alone (`waiting`, `finishing`,
  `connected` or `failed`, with a fixed `reason`); `POST /v1/accounts/oauth/callback` takes the
  mailbox's first key (`public_key`, `namespace`, `grant`) with a link's return, and may take up to
  70 s to answer it.

### The open console leaves

No migration: the schema does not change, and going back is the binary (and its console) you kept.
The self-hosted server has no web console any more: it is run from its command line and used
through REST with keys.

- **Nobody signs in.** The image has no console and sets no `MAIL_WEB_DIR`, `compose.yaml` keeps
  it empty, and `deploy/mailie.service` no longer sets it: the daemon serves the API only, and
  `GET /` answers 404. Install the new unit once (`install -m 0644 deploy/mailie.service
  /etc/systemd/system/mailie.service`, then `systemctl daemon-reload`): the older one keeps serving
  the old console's files, which no longer match the API. Remove the console's files
  (`/usr/local/share/mailie/console`) once you will not go back.
- **People's mailboxes stay where they are.** A mailbox linked in the console belongs to its
  person's or its team's workspace, which no instance key reaches. It keeps syncing under the
  consent it was given, and the workspace keys made for it work until they expire or are revoked,
  but nobody signs in to change it. At every start the daemon logs, at warn, `mailboxes outside the
  operator workspace, in personal or team workspaces: no instance key reaches them`, with their
  counts (`personal_live`, `personal_removed`, `team_live`, `team_removed`: removed ones are kept
  until they are deleted for good) and a hint naming the README's section
  [Moving from the open console](../README.md#moving-from-the-open-console), which says how to link
  each address again into the operator workspace and then delete the people. It logs nothing when
  there are none, nor while `MAIL_PASSWORD_SIGN_IN` lets people sign in, whose workspaces those
  are. A mailbox linked through a web client refreshes only with it: keep its variables,
  and `MAIL_PUBLIC_URL`, until it is linked again, and remove them after.
- **The command line.** `account add --password` takes only `-`: the password is typed at a prompt
  that does not show it, or is the first line of standard input (its line ending alone taken off,
  at most 4096 bytes). A script that passed it as an argument is refused before anything is sent:
  `account add: --password takes no password on the command line, which sudo, systemd-run and shell
  history record; give --password - and pipe the password on standard input, or type it at the
  prompt; nothing was added`. `account add` and `account authorize` say where the mailbox's sync
  stands (``its sync is off until `mailserver account sync ID on` ``), where `account add` said it
  was ready to sync.
- **For scripts:** `GET /v1/me/storage` counts each mailbox's `bytes` by usage rule 1
  (`rule_version`; [`console.md`](console.md#storage)), what the database holds for it, where it
  summed the mail server's sizes, which are now `provider_bytes`; it lists a removed mailbox
  (`removed: true`) to whoever lists it on the removed mailboxes' own paths, until it is deleted for
  good; and it answers `database_bytes` only to the operator (an instance key with the admin scope,
  restricted to no mailbox), no longer to an owner signed in. The refusal of a key nobody agreed to
  the key terms through names no console.

### No people and no MCP of its own

No migration: the schema does not change, and going back is the binary you kept. A self-hosted
server has no people and serves no MCP of its own (platform decision 0065): its operator acts with
instance keys made on its command line, and tools use REST with them, as before.

- **No people.** `MAIL_PASSWORD_SIGN_IN` (default `false`) says whether people sign in with a
  password here, and a self-hosted server leaves it unset. Off, the routes that sign in, sign up,
  recover or reset a password and create an invitation answer `403` (`this server has no people:
  nobody signs in, signs up or is invited here; use an instance key made with its command line`),
  to an admin key too; a session token is refused as an unknown one is (`401`), so a person still
  signed in from the open console is signed out at their next request; the daemon makes and opens
  no salt key (one a database holds is left as it is, and `rewrap-credentials` still re-seals it);
  and the start says nothing of people: no `nobody can sign in to the console yet`, no `nobody
  administers this server`. Without any key it warns `no api keys exist yet; nothing can call this
  daemon`, with the `apikey create --bootstrap` hint. The workspace keys made in the open console
  work as before, until they expire or are revoked ([No people, and no
  passwords](#no-people-and-no-passwords)).
- **The command line invites nobody.** `user invite` and `user password` are refused, with no
  database opened and nothing asked of the daemon: `user invite: the command line invites nobody
  and resets no password: the operator acts with instance keys, which apikey create makes` (`user
  password: …` for the other). `workspace create`, `member role` and `access grant` are gone (the
  usage, exit 2). What stays lists and takes away, for a database the open console left:
  `user disable|delete [--force] [--bootstrap]`, `workspace list|rename|close|reopen`, `member
  list|remove` and `access list|revoke`.
- **`account authorize` of a mailbox that works** (`active`) opens the consent page and returns,
  saying that the mailbox keeps the grant it has until a new one logs in and that the browser's
  page says how the consent ends: nothing tells the command line how a consent for a working
  mailbox ends. `--device` on such a mailbox is refused before anything starts; `--paste` waits for
  the outcome, as before. A mailbox that failed or waits for its authorization is waited for as
  before.
- **No MCP.** Every method on `/mcp` and below answers the API's JSON `404`; `serve` takes no flag,
  so a unit or a client that runs `serve --mcp-stdio` stops at once (`flag provided but not
  defined: -mcp-stdio`); `mailserver mcp connect|install` exits 1 saying the server serves no MCP of
  its own; `GET /v1/me/mcp` is gone, and so are the API's own `/.well-known/oauth-*` mounts (both
  still `404`). `MAIL_MCP_HTTP`, `MAIL_MCP_KEY` and `MAIL_MCP_ELICIT_SEND` are read by nothing, and
  one that still asks for MCP is named once at start, at warn
  ([`mcp.md`](mcp.md#the-warning-at-start)). Remove them, remove this server from every MCP client
  configured for it, and revoke the keys only those clients held. The proxy needs no change.
- **For scripts:** codes do not change. Some refusals now say who may change something, never
  where: `actions are off: the person who asks, or who created this key, has not allowed them, and
  only that person can`, `sending is off: you have not allowed it, and only you can` and `sync is
  off: its person has not turned it on, and only they can`.

```sh
# Compose, from deploy/:
docker tag mailie:local mailie:previous    # the image running now, to go back to
git pull
docker compose build                       # the new image, while the old one still runs
docker compose stop mailie
docker compose cp mailie:/data ~/mailie-before-upgrade && chmod 700 ~/mailie-before-upgrade
docker compose up -d

# systemd:
sudo systemctl stop mailie
sudo cp -a /var/lib/mailie /var/lib/mailie.before-upgrade
sudo cp -a /usr/local/bin/mailserver /usr/local/bin/mailserver.previous
sudo install -m 0755 dist/mailserver /usr/local/bin/mailserver
sudo systemctl start mailie
```

Remove the last upgrade's copies first: `cp` into a directory that exists puts the copy inside it.

### Going back

Stop the daemon, put the copy back as the daemon's own (without the newer version's `mail.db-wal`
and `mail.db-shm`, which belong to the database being replaced), and start the version you kept:

```sh
# Compose, from deploy/. The image has no shell, so a throwaway container puts the copy back,
# owned by the image's user (65532); it does nothing unless the copy holds a database.
docker compose stop mailie
docker run --rm -v mailie_data:/data -v "$HOME/mailie-before-upgrade:/before:ro" alpine:3 \
  sh -c 'test -s /before/mail.db && rm -f /data/mail.db-wal /data/mail.db-shm &&
         cp -a /before/. /data/ && chown -R 65532:65532 /data'
docker tag mailie:previous mailie:local
docker compose up -d
git checkout <the commit you upgraded from>   # git pull printed it; the next build is then this version

# systemd:
sudo systemctl stop mailie
sudo rsync -a --delete /var/lib/mailie.before-upgrade/ /var/lib/mailie/
sudo install -m 0755 /usr/local/bin/mailserver.previous /usr/local/bin/mailserver
sudo systemctl start mailie
```

The copy is the database in clear apart from the credentials: delete it, and the kept version, once
the new version has proved itself. Going back from the release without the open console to one with
it puts back that release's unit as well (`deploy/mailie.service` at the commit you upgraded from),
which names the console's files, and those files if you removed them. Going back from the release
with no people and no MCP to one before it serves `/mcp` again unless `MAIL_MCP_HTTP=false`, and
signs people in with a password again, as that release did.

Going back from the release that brings the key scheme (migration 0013) puts back the old password
hashes and drops every enrolment made since. When you upgrade again, to a release after the one that
brought the scheme, which has no upgrade, those people sign in no more: the releases that had people
needed a reset invitation for each, and on this release nobody signs in
([No people, and no passwords](#no-people-and-no-passwords)); upgrading again to that release itself, the
browsers of the people who signed in on it remembered that they enrolled and refused the upgrade,
with that release's remedy, a reset invitation, or clearing the site's data in their browser.

## No people, and no passwords

A self-hosted server has no people: nobody signs in to it, and the command line and every tool
authenticate with keys. No password of anyone's is used there, and nothing on it, the command line
included, invites a person or prints a reset invitation (platform decision 0065). With
`MAIL_PASSWORD_SIGN_IN` unset, the routes that would answer `403` (`this server has no people: …`)
and a session token `401`, as [the release that took them away](#no-people-and-no-mcp-of-its-own)
says. `MAIL_PASSWORD_SIGN_IN=true` is for an edition that serves a page where people sign in with a
password ([`key-scheme.md`](key-scheme.md)); a self-hosted server serves none, so it changes nothing
a person could use there.

A database made through the open console of an earlier release still holds its people, their
account keys and their recovery codes, and its mailboxes in their workspaces, which no instance key
reaches. A forgotten password is nothing to recover there: to reach a person's mailboxes, link
them again into the operator workspace, then delete the people
([Moving from the open console](../README.md#moving-from-the-open-console)). A reset invitation an
earlier release printed opens nothing here.
