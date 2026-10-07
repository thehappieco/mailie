# Self-hosting

Mailie runs as one process: a static binary, one SQLite database in a data directory, and the
built console it serves as static files. This page is how to run it on a server of your own, from
a binary under systemd or in a container, behind a reverse proxy that terminates TLS. The packaging
is in [`deploy/`](../deploy): a `Dockerfile`, a `compose.yaml` and example systemd units.

- [Requirements](#requirements)
- [The configuration a server needs](#the-configuration-a-server-needs)
- [With Docker Compose](#with-docker-compose)
- [With Docker alone](#with-docker-alone)
- [From a binary, with systemd](#from-a-binary-with-systemd)
- [TLS through a reverse proxy](#tls-through-a-reverse-proxy)
- [The first owner](#the-first-owner)
- [Mailboxes and OAuth clients](#mailboxes-and-oauth-clients)
- [MCP clients](#mcp-clients)
- [Health, metrics and logs](#health-metrics-and-logs)
- [Backups](#backups)
- [Upgrades](#upgrades)
- [A forgotten password](#a-forgotten-password)

## Requirements

- **A Linux machine** (amd64 or arm64) that can reach your mail providers: IMAP on 993, SMTP on
  465 or 587, and their OAuth endpoints on 443. From outside, only the reverse proxy's 443 (and 80)
  need to be open.
- **A name and a certificate.** The console and MCP clients reach the daemon through a reverse
  proxy that terminates TLS (Caddy, nginx); the daemon itself only speaks plain HTTP on loopback.
- **To build it:** Docker with BuildKit (any current Docker Engine or Docker Desktop) builds both
  the binary and the console in the image. Without Docker, Go 1.27 and Node 24.
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
| `MAIL_CREDENTIAL_KEY_HEX` | Required. `openssl rand -hex 32`. It encrypts every stored refresh token and password. **Keep a copy with your other secrets: losing it means authorizing every mailbox again.** |
| `MAIL_PUBLIC_URL` | The `https://` origin people reach the console at, such as `https://mail.example.org`: no path, no trailing slash. Invite links and the web OAuth redirect (`<MAIL_PUBLIC_URL>/oauth/return`) are built from it, never from a request's `Host`. |
| `MAIL_ENV` | `prod`: logs in JSON, and refuses an `http://` `MAIL_PUBLIC_URL` and the settings that would leak a token. |
| `MAIL_TRUSTED_PROXIES` | The address the daemon sees your reverse proxy connect from (see [below](#the-proxys-address)). Without it every client shares one rate limit. |
| `MAIL_GOOGLE_WEB_CLIENT_ID`, `_SECRET`; `MAIL_MICROSOFT_WEB_CLIENT_ID`, `_SECRET` | The web OAuth clients, for Gmail and Microsoft mailboxes ([below](#mailboxes-and-oauth-clients)). iCloud and IMAP mailboxes need none. |

Three more say where things are: `MAIL_DATA_DIR` (the database), `MAIL_HTTP_ADDR` (what the daemon
listens on) and `MAIL_WEB_DIR` (the built console). The image and the systemd unit set them; leave
them out of your environment file.

## With Docker Compose

[`deploy/compose.yaml`](../deploy/compose.yaml) builds the image from your checkout and runs it
with its root file system read-only, no capabilities, `no-new-privileges`, the database on a named
volume (`mailie_data`) and the port published on the host's loopback only (`127.0.0.1:8080`).

```sh
cp .env.example deploy/.env
chmod 600 deploy/.env
# Edit deploy/.env: MAIL_CREDENTIAL_KEY_HEX, MAIL_PUBLIC_URL, MAIL_ENV=prod, MAIL_TRUSTED_PROXIES.
cd deploy
docker compose build
docker compose run --rm mailie user invite --bootstrap --role owner --email you@example.com   # the first owner
docker compose up -d
curl -fsS http://127.0.0.1:8080/v1/healthz
```

`deploy/.env` holds the credential key: Git ignores it, and it never enters the image (the build
context lets in only `go.mod`, `go.sum`, `cmd/`, `internal/` and `web/`). Compose reads the file
itself, so write values without quotes unless a value holds a `$`, and then in single quotes.

The rest of this page writes the commands from `deploy/`:

| What | How |
|---|---|
| A command that talks to the running daemon (`account`, `user invite`, and `apikey create\|list\|revoke` with `MAIL_ADMIN_API=true`) | `docker compose exec -e MAIL_ADMIN_KEY mailie mailserver account list`, with `MAIL_ADMIN_KEY` in your shell |
| A command that needs the daemon stopped (`--bootstrap`, `migrate`, `rewrap-credentials`) | `docker compose stop mailie`, then `docker compose run --rm mailie <command>`, then `docker compose start mailie` |
| The logs | `docker compose logs -f mailie` |

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
values bare, comments on lines of their own. Keep
`MAIL_DATA_DIR`, `MAIL_HTTP_ADDR` and `MAIL_WEB_DIR` out of that file, so the image's
(`/data`, `0.0.0.0:8080`, `/usr/share/mailie/console`) apply.

What the image is:

- **Three stages.** Node 24 builds the open console, Go 1.27 builds a static binary
  (`CGO_ENABLED=0`, `-trimpath`, the version stamped in), and both are copied onto distroless's
  `static-debian13` image, which holds CA certificates, time zones and little else: no shell, no
  package manager. Every base image is pinned by digest.
- **It runs as `nonroot` (65532:65532)**, and the binary and the console belong to root. `/data` is
  the only place it writes, besides `/tmp`; it is `0700` and the daemon's, and a new named volume
  takes that owner and mode. A bind mount needs the same: `chown 65532:65532` and `chmod 700` on
  the host directory.
- **No secret is built in.** Everything comes from the container's environment when it starts.
- **No `HEALTHCHECK`:** the image has no tool to run one. Check from outside (see
  [Health](#health-metrics-and-logs)).
- On `SIGTERM` the daemon closes its mailbox connections and exits within 15 seconds; Compose and
  the command above allow 30.

## From a binary, with systemd

Build the binary and the console on any machine with Go 1.27 and Node 24. The binary is static, so
it can be built elsewhere for the server's platform:

```sh
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.version=$(git describe --always --dirty)" -o dist/mailserver ./cmd/mailserver
make web-install web-build      # the console, into web/dist
```

Or take both out of the image (`docker build --platform linux/amd64 …` for another machine):

```sh
mkdir -p dist web/dist
docker create --name mailie-files mailie:local
docker cp mailie-files:/usr/local/bin/mailserver dist/mailserver
docker cp mailie-files:/usr/share/mailie/console/. web/dist/
docker rm mailie-files
```

On the server, as root, with [`deploy/mailie.service`](../deploy/mailie.service):

```sh
useradd --system --user-group --home-dir /var/lib/mailie --no-create-home --shell /usr/sbin/nologin mailie
install -d -o mailie -g mailie -m 0700 /var/lib/mailie
install -m 0755 dist/mailserver /usr/local/bin/mailserver
mkdir -p /usr/local/share/mailie/console && cp -R web/dist/. /usr/local/share/mailie/console/
install -d -m 0700 /etc/mailie
install -m 0600 .env.example /etc/mailie/mailie.env
# Edit /etc/mailie/mailie.env: MAIL_CREDENTIAL_KEY_HEX, MAIL_PUBLIC_URL, MAIL_ENV=prod, MAIL_TRUSTED_PROXIES.
install -m 0644 deploy/mailie.service /etc/systemd/system/mailie.service
systemctl daemon-reload
```

The unit runs the daemon as `mailie` with the database in `/var/lib/mailie`, listening on
`127.0.0.1:8080`. The environment file belongs to root, `0600`: systemd reads it before the daemon
starts, so the daemon's user cannot read the file itself. systemd reads that file without a shell:
one `NAME=value` per line, and no comment after a value.

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
daemon stopped (`systemctl stop mailie`); the ones that talk to it (`account`, `user invite`, and
`apikey create|list|revoke`) need it running and the admin key in `/etc/mailie/admin.env`. The
`apikey` ones also need `MAIL_ADMIN_API=true` in `/etc/mailie/mailie.env` (off by default);
without it, `apikey create --bootstrap` issues a key with the daemon stopped. With `--pty` a
command can ask for a password at your terminal; to pipe its input instead, use `--pipe`.

## TLS through a reverse proxy

The daemon speaks plain HTTP. **Never expose port 8080:** put a reverse proxy on the same machine
in front of it, publish only 443 (and 80, for the redirect and certificate issuance), and set
`MAIL_PUBLIC_URL` to the `https://` origin the proxy serves.

What the proxy must do:

- **Pass streams through.** `/v1/events` (Server-Sent Events) and `/mcp` stream; a buffered response
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

## The first owner

The first person is invited from the command line, with the daemon stopped, as an owner: the
invite decides the role, and signing up first makes nobody an owner. Set `MAIL_PUBLIC_URL` first:
the invite link is built from it.

```sh
# Compose, from deploy/:
docker compose stop mailie
docker compose run --rm mailie user invite --bootstrap --role owner --email you@example.com
docker compose start mailie

# systemd, with the helper above:
sudo systemctl stop mailie
mailie-admin user invite --bootstrap --role owner --email you@example.com
sudo systemctl start mailie
```

Open the link it prints, choose a name and a password, and invite the others from the console or
with `mailserver user invite --email ADDRESS [--role owner|member]` beside the running daemon. An
admin key for the command line comes the same way:
`mailserver apikey create --bootstrap --scope admin --name cli`, with the daemon stopped (with
systemd, `mailie-admin apikey create --bootstrap …`, and the key it prints goes in
`/etc/mailie/admin.env`).

## Mailboxes and OAuth clients

iCloud and other IMAP mailboxes connect with a password and need nothing more. Gmail and
Microsoft mailboxes need OAuth clients you register yourself, for your server: the
[README](../README.md#mailboxes-and-providers) walks through both providers.

On a server, the console uses the **web clients** (`MAIL_GOOGLE_WEB_CLIENT_*`,
`MAIL_MICROSOFT_WEB_CLIENT_*`), each registered with the redirect
`https://mail.example.org/oauth/return`: your `MAIL_PUBLIC_URL` followed by `/oauth/return`,
exactly. The installed clients (`MAIL_GOOGLE_CLIENT_ID`, `MAIL_MICROSOFT_CLIENT_ID`) redirect to a
listener the daemon opens on its own loopback, which a browser on another machine, or outside the
container, cannot reach: on a server they serve only the command line's `account add --paste`.

## MCP clients

MCP is served at `<MAIL_PUBLIC_URL>/mcp` through the same proxy, with the API keys the owners and
admins of a workspace create in the console. From the machine an MCP client runs on:

```sh
mailserver mcp install --client claude-desktop --url https://mail.example.org
```

[`docs/mcp.md`](mcp.md#connect-a-client) covers every client, `mcp connect`, and what each one
writes where.

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
inconsistent. Either way, a backup holds the people, the index and the credentials encrypted under
`MAIL_CREDENTIAL_KEY_HEX`, which is not in it: keep the key apart, and the copy as private as the
data directory.

## Upgrades

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
quick start of earlier releases had you make for yourself. If the daemon logs at start that nobody
administers the server, invite an owner, with an address that has no account yet.

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
sudo cp -a /usr/local/share/mailie/console /usr/local/share/mailie/console.previous
sudo install -m 0755 dist/mailserver /usr/local/bin/mailserver
sudo rsync -a --delete web/dist/ /usr/local/share/mailie/console/
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
sudo rsync -a --delete /usr/local/share/mailie/console.previous/ /usr/local/share/mailie/console/
sudo systemctl start mailie
```

The copy is the database in clear apart from the credentials: delete it, and the kept version, once
the new version has proved itself.

## A forgotten password

Nobody can set a password through the API. The operator resets one with the daemon stopped;
`user password --bootstrap` asks for the new password twice at a terminal (or reads one line piped
in) and ends every session the person has.

```sh
# Compose, from deploy/:
docker compose stop mailie
docker compose run --rm mailie user password --bootstrap --email you@example.com
docker compose start mailie

# systemd, with the helper above:
sudo systemctl stop mailie
mailie-admin user password --bootstrap --email you@example.com
sudo systemctl start mailie
```
