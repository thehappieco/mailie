# MCP

The daemon speaks the Model Context Protocol, so an AI assistant (Claude Code, for one) or any other
MCP client can search and read the mailboxes a key of their workspace holds and, with a write key,
act on them. It is the
same service as the REST API: the same rules about who sees what, the same search of the index, the
same bodies fetched from the mail server on request and never stored, the same actions behind the
same consent. No authorization rule lives in `internal/mcp`.

- **Endpoint:** `/mcp` on the daemon's listener (`http://127.0.0.1:8080/mcp` by default, or
  `https://<your host>/mcp` behind your proxy): Streamable HTTP, **stateful** (POST, GET and
  DELETE). On unless `MAIL_MCP_HTTP=false`.
- **stdio:** `mailserver mcp connect --url <server>`, a local server for a client that launches one,
  relaying to a server's `/mcp` (on any machine); or `mailserver serve --mcp-stdio`, for a client that
  launches the whole daemon itself. See [Connect a client](#connect-a-client).
- **Credential:** an API key in `Authorization: Bearer <key>`. There is no OAuth.

## Who gets in, and what they see

- **Keys only.** `/mcp` refuses a console session token (`401`): a session is a person in a
  browser, and copying it into an assistant's configuration must not work.
- **A workspace's key.** A tool reaches mail only with a key of the mailbox's workspace, which an
  owner or an admin of it created in the console, under the workspace's API keys (in a personal
  workspace, its person), with the scope and lifetime they chose. Creating it is their agreement to
  the key terms, which say what a tool holding it can do, and the key records the revision of the
  text they saw (`terms_version`). A key nobody agreed to the terms through is refused here and over
  REST (`403`); the upgrade to workspace keys (migration 0012) revoked every one.
- **What a key reaches** is exactly what it holds, read at each call: the mailboxes of its
  workspace it was given, each with `read`, `act` or `send` (`docs/workspaces.md`, "API keys").
  `read` is given only by an owner or an admin who reads the mailbox, so a key never reads more
  than someone who reads it decided; what it holds stands on its own, whoever gave it. A mailbox
  taken out of the key, or the key revoked, is gone from the very next call, also for what holds
  the key for long — a `serve --mcp-stdio` session, the resource subscriptions of any session —
  and a mailbox given to it is there at the next call. Every account a tool lists carries its
  `workspace_id` and what the key may do with it (`access`). Every owner and admin of the workspace
  sees the key and may revoke it; it also stops when the person who created it leaves the
  workspace or is disabled or deleted.
- **Instance keys** (the operator's, from `mailserver apikey create`) reach only the operator
  workspace's mailboxes, here and over REST alike, never a workspace's. One restricted to some of
  them that loses one it was opened with is refused at every call of a stdio session with
  `conflict: this key no longer reaches a mailbox it was authenticated with`: restart it.
- **Scopes:** `read` searches and reads; `write` also marks read, stars, moves, archives and
  trashes. Actions still need the `act` flag the key holds on the mailbox, checked before every
  command that changes the mailbox, as over REST; the key terms cover them, and no person's own
  permission is asked. A key with the `send` scope sends over REST; there is no send tool yet.
- **Every request and every tool checks the key again.** Each HTTP request is authenticated in full,
  through the same rate limiter as REST. Each tool reads the key again before it starts and before
  it answers, so a key revoked mid-call never sees its result, and a revoked key loses its session
  at the next call. A session belongs to the key that opened it: another key presenting the same
  `Mcp-Session-Id` gets `403`.
- **Origin.** A request whose `Origin` is another site is refused (`403`), against DNS rebinding.
  Pages on this machine (`localhost`, `127.0.0.1`, `::1`) and the console's own origin
  (`MAIL_PUBLIC_URL`) are allowed. The SDK's own localhost protection is off, because behind a
  reverse proxy every request reaches the loopback listener with the public `Host`.
- `/.well-known/oauth-*` answer `404` and a `401` carries no `resource_metadata`, so a client uses the
  key it was given instead of starting an OAuth discovery that cannot succeed.

## Tools

| Tool | What it does | Annotations |
|---|---|---|
| `list_accounts` | the mailboxes the key reaches: id, address, provider, state, sync, what the key may do with each (`access`; a team mailbox its owner or admin manages without reading is listed, marked without read access), which actions are available | read |
| `list_folders{account}` | folders with id, role and counts; for a mailbox not synced yet, asked of the mail server | read, open world |
| `search_messages{account?, folder?, q?, from?, since?, until?, unseen?, flagged?, has_attachments?, limit 1–100 (20), cursor?}` | searches the index, newest first; `q` matches subject, sender and recipients, never the body | read |
| `get_message{id, format: text\|html\|both (text), max_bytes (65536, at most 5 MiB)}` | the message, fetched from the mail server now; not marked as read, nothing stored | read, open world |
| `get_attachment{id, part, max_bytes ≤ 1 MiB}` | the attachment, embedded as base64; a larger one is not fetched: `{too_large, size, download_path}` | read, open world |
| `wait_for_new_mail{account?, since_cursor?, timeout_seconds 1–230 (45)}` | waits for new mail and returns `{timed_out, lagged, messages[], next_cursor}`; waiting in vain is not an error | read |
| `mark_read{ids[1..100], read (true)}` | marks read or unread on the server | write, not destructive, idempotent |
| `flag_message{ids, flagged (true)}` | stars or unstars | write, not destructive, idempotent |
| `move_message{ids, to: archive\|inbox\|<folder id>}` | moves or archives; not to the trash | write, not destructive, idempotent |
| `trash_message{ids}` | moves to the trash, which is reversible | write, **destructive**, `_meta["anthropic/requiresUserInteraction"]` |

Read tools are `readOnlyHint: true`, `destructiveHint: false`, `idempotentHint: true` and
`openWorldHint: false`, except `get_message` and `get_attachment`, which go to the mail server, and
`list_folders`, which does when the mailbox has no folders in the index (not synced yet, or no
consent to sync). The `account` argument has the shape of an account id in the schema
(`^acc_[0-9a-f]{16}$`): free text in its place is refused before the tool runs. Write tools change
the real mailbox (`openWorldHint: true`). The trash is a tool of its own because annotations are per
tool: it is the one a client should confirm. claude.ai and Claude Desktop confirm what is
`destructiveHint`; Claude Code confirms what has `requiresUserInteraction`.

Every result has `structuredContent` and a text. The text is what a model reads in clients that
pass only `content` along, so `get_message`'s carries the body and `search_messages`'s one line per
message. A service error becomes a tool error with the text `code: message`, using the REST API's
seven codes and never the mail server's words; `rate_limited` says how many seconds to wait.

`wait_for_new_mail` waits in steps of 25 seconds. Between steps it checks the key again and, when the
client sent a `progressToken`, sends a progress notification (which also keeps an idle connection
alive). The 230-second maximum stays under the 240 seconds claude.ai and Claude Desktop give a tool
call. The cursor carries from step to step, so nothing that arrives between two steps is lost.

## Resources

| URI | Content |
|---|---|
| `mail://accounts` | the key's mailboxes, as `list_accounts` |
| `mail://{account}/folder/{folder}` | the folder's 50 newest messages, from the index; `folder` is an id or a role (`inbox`, `sent`, `trash`, …) |
| `mail://{account}/message/{id}` | the message with its text body (up to 64 KiB), fetched from the server; not marked as read |

`resources/subscribe` accepts only `mail://{account}/folder/inbox` of a mailbox the key may read
(a grant that only shows the mailbox is not enough), and sends `notifications/resources/updated`
when new mail reaches that inbox. It is computed with the session's own key, so it never speaks of
another mailbox, and it stops when the key stops working: the key is checked before each wait on
the journal and again before announcing what arrived during it, and so is read access to each
inbox subscribed to, so one the key can no longer read is dropped rather than announced. Few clients act on that notification today; `wait_for_new_mail` is what hands new mail to a
model.

Every result carries `cacheScope: "private"` and `ttlMs: 0`. The protocol's default, `"public"`,
would let an intermediary keep a response and serve it to someone else, and this is one key's mail.

## Connect a client

A client needs two things: the server's address, and an API key. An owner or an admin of a
workspace creates one in the console, under the workspace's API keys, for the mailboxes, scope and
lifetime they choose (in a personal workspace, its person does); the operator's instance keys come
from `mailserver apikey create`. The key is the only credential: whoever holds it
reaches what it reaches, so it goes where only its owner can read it, never into a command line
someone else can see, an address or a log.

| Client | How it reaches the server | Set up with | Where the key ends up |
|---|---|---|---|
| Claude Code | HTTP, the key in a header | `claude mcp add` (below), which you run | `~/.claude.json`, Claude Code's own file |
| Cursor | HTTP, the key in a header | `mailserver mcp install --client cursor` | `~/.cursor/mcp.json`, mode 0600 |
| Claude Desktop | `mailserver mcp connect`, launched by it | `mailserver mcp install --client claude-desktop` | its `claude_desktop_config.json`, mode 0600 |
| Any other client that launches a local server | `mailserver mcp connect` | its own configuration, as below | the environment it gives `mcp connect` |
| Any other client that sends a fixed `Authorization: Bearer` header | HTTP | its own configuration | its configuration |

Every way needs MCP over HTTP on the server (the default; not `MAIL_MCP_HTTP=false`), and an
`https://` address, or `http://` to this very machine (`localhost`, `127.0.0.1`, `::1`): over plain
HTTP to anywhere else the key would cross the network in clear, and the commands refuse it.
**claude.ai web connectors** and Claude Desktop's *remote* connectors need OAuth, which Mailie does
not offer yet; `mcp connect` is how Claude Desktop reaches a Mailie server meanwhile.

### Claude Code

Claude Code speaks MCP over HTTP with a header itself:

```sh
claude mcp add --transport http --scope user mailie https://mail.example.com/mcp \
  --header "Authorization: Bearer <your key>"
```

The console shows this command with the server's own address beside a workspace's API keys, and a
new key's dialog copies it with the key in it, whenever the server answers MCP over HTTP.
`mailserver mcp install --client claude-code --url https://mail.example.com` checks that the address
answers MCP and prints the same command with `<your key>` where the key goes, and writes nothing:
Claude Code keeps its servers in `~/.claude.json`, its own state file, which it rewrites while it runs
and whose format its documentation does not offer to other programs, and `claude mcp add` takes the
header only on its command line, where other programs on the machine can read it while it runs. So
the command is yours to run; clear it from your shell's history afterwards. `claude mcp remove --scope
user mailie` removes it (`mcp install --client claude-code --uninstall` prints that too).

### `mailserver mcp connect`

```sh
MAILIE_API_KEY=<your key> mailserver mcp connect --url https://mail.example.com
```

A local MCP server on standard input and output, for a client that launches one, that relays every
message to the server's `/mcp` over HTTPS with the key from `MAILIE_API_KEY`: the client talks to
the Mailie server as if it were local. It is the same binary as the daemon but runs nothing of it:
no database, no sync, no `MAIL_*` variable and no `.env`, so it runs on any machine — a laptop
reaching a server elsewhere, or beside a daemon on the same one (unlike `serve --mcp-stdio`, which is a
whole daemon).

- **The key** comes from `MAILIE_API_KEY` and nowhere else; there is no flag for it. Without it, or
  with something that is not an API key (a console session token, for one), `mcp connect` refuses to
  start. It sends the key only to the address given, and never follows a redirect, which would take
  the key along.
- **What passes:** everything. The bridge copies JSON-RPC messages both ways without reading them:
  tools, resources, subscriptions and their `notifications/resources/updated`, progress, the server's
  own requests to the client and the client's answers. It is the go-sdk's own stdio transport towards
  the client and its Streamable HTTP client towards the server, plus the two things that client does
  only inside the SDK's own session: it sends the negotiated `MCP-Protocol-Version` on every request
  of the session, and it holds the session's GET stream, where the server sends what it says outside a
  request (a subscription's updates).
- **A session the server ended** — idle for 30 minutes, the key's least recently used of 16, a
  daemon that restarted — is opened again with the client's own `initialize`, and its subscriptions
  made again, before the request that found it gone is sent again: the client never sees it. A call
  still waiting when its session ended is answered with an error instead of never.
- **The stream of notifications** (the session's GET stream) is asked for again whenever it ends,
  with `Last-Event-ID`, so that the server replays what it sent meanwhile. A refusal is asked again
  too, waiting longer each time (1 s, doubling up to 30 s): a server restarting (`5xx`), limiting the
  key (`429`), or still holding the stream for a connection it has not noticed is dead (`409`: the
  client's network changed or its machine slept, and the server, or a proxy in front of it, still
  believes the old connection is there). When the server still holds it after 3 minutes, or can no
  longer replay what it sent meanwhile (`400`: it keeps that for 5 minutes at most), the bridge ends
  the session and opens another with the client's subscriptions, as above: what was sent in the gap
  is lost, the bridge logs that, and a call still waiting in the old session is answered with an
  error. Only a server without that stream (`405`) is left without one.
- **Logs** go to standard error (standard output is the client's), one line per event of its own,
  never a message's content and never the key.
- **It ends** when the client closes standard input (exit 0), and with a line on standard error and a
  non-zero exit when the server refuses the key (`401`: wrong, expired or revoked — at the start, or at
  the first request after it was revoked), refuses it MCP (`403`: a key nobody agreed to the key
  terms through), has no MCP endpoint at that address (`404`: not a Mailie server, or `MAIL_MCP_HTTP=false`
  there) or answers with a redirect — also when the client, told of it, closes standard input at
  once. A client shows that line in its log of the server.

For a client that launches a local server, the entry is the one `mcp install` writes for Claude
Desktop:

```json
{
  "mcpServers": {
    "mailie": {
      "command": "/usr/local/bin/mailserver",
      "args": ["mcp", "connect", "--url", "https://mail.example.com"],
      "env": { "MAILIE_API_KEY": "<your key>" }
    }
  }
}
```

### `mailserver mcp install`

```sh
mailserver mcp install --client claude-desktop --url https://mail.example.com
mailserver mcp install --client cursor --url https://mail.example.com [--name mailie] [--force]
mailserver mcp install --client claude-desktop --uninstall [--name mailie]
```

It asks for the key at the terminal without showing it, or reads one line of standard input when that
is not a terminal (`mailserver mcp install … < key.txt`), and opens a session with it before anything
is written: a wrong key, a key that may not use MCP, an address with no MCP endpoint or a binary that
`go run` built (gone by the time Claude Desktop launches it) are refused, and nothing is written.

| Client | File | Entry |
|---|---|---|
| `claude-desktop` | `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS), `%APPDATA%\Claude\claude_desktop_config.json` (Windows), `$XDG_CONFIG_HOME/Claude/` or `~/.config/Claude/claude_desktop_config.json` (Linux) | `{command: <this binary's absolute path>, args: ["mcp","connect","--url",URL], env: {MAILIE_API_KEY: key}}` |
| `cursor` | `~/.cursor/mcp.json` | `{url: URL/mcp, headers: {Authorization: "Bearer " + key}}` |
| `claude-code` | none | the `claude mcp add` command above, printed |

Every write:

- changes the named server (`--name`, default `mailie`) under `mcpServers` and nothing else: other
  servers and settings keep their values and their order (the file is laid out again, two spaces);
- refuses to replace a server of the same name without `--force`, before the key is asked for;
- refuses a file that is not a JSON object, rather than replace it;
- refuses a symbolic link anywhere below your home directory on the way to the file — the file
  itself, or a directory such as `~/.cursor`, `~/.config` or `~/Library` — which often leads into a
  repository of dotfiles where a key has no business (a link above the home, such as a `/home` that
  is one, is the system's and is followed);
- writes a temporary file and renames it over the original, so a crash leaves the old file or the new
  one; the file holds a key now, so it gets mode 0600;
- keeps the file as it was before mailserver first changed it in `<file>.bak-mailie`, once, and mode
  0600: later writes never replace that copy, and when there was no file the copy is left empty,
  which records that, so it never holds a key mailserver wrote;
- says which file changed, and never the key.

`--uninstall` removes the named server and nothing else, and says so when there is none. It keeps no
copy (the file it changes holds the key it removes), and since it writes no key it goes through a
linked directory; a linked file is still refused, because the rename would replace the link. Claude
Desktop's entry names this binary's absolute path: if the binary moves, run `mcp install --force`
again. Restart the client after a change.

### Where the key ends up, and taking it back

The key sits in the client's file (Cursor's, Claude Desktop's, readable only by you after `mcp
install`) or in Claude Code's. Removing the entry does not make the key stop working: revoking it does.
An owner or an admin of its workspace revokes it in the console, under the workspace's API keys, and
the person who created it under their own keys in their account; the operator revokes any key with
`mailserver apikey revoke PREFIX`. A revoked key stops at its next request — over HTTP the client gets
`401`, and `mcp connect` ends with a line saying so — and `mcp install --uninstall` then clears the
entry.

### `serve --mcp-stdio`

For a client that launches the whole daemon itself, such as Claude Desktop's
`claude_desktop_config.json` on the server's own machine:

```json
{
  "mcpServers": {
    "mailie": {
      "command": "/path/to/mailserver",
      "args": ["serve", "--mcp-stdio"],
      "env": {
        "MAIL_MCP_KEY": "<your key>",
        "MAIL_CREDENTIAL_KEY_HEX": "…",
        "MAIL_DATA_DIR": "…"
      }
    }
  }
}
```

This runs the whole daemon (sync included), so it cannot share a data directory with another
running daemon: the second refuses the lock. With `--mcp-stdio` every log line goes to standard
error (standard output is the client's), `MAIL_MCP_KEY` is required and checked like a bearer key
when the daemon starts, and the daemon exits when the client hangs up. The session holds that key
as it was when the daemon started: once the key's restriction loses a mailbox (its person lost
`read` on it), every call answers `conflict`, and the client has to restart the server to go on with
what the key still names. Beside a daemon that is already running, use `mcp connect` instead.

### Switching HTTP off

`MAIL_MCP_HTTP=false` (default `true`) leaves `/mcp` unmounted: every method
on `/mcp` and under it answers the API's JSON `404` (`{"code":"not_found","message":"no such
endpoint"}`), with or without a console, and no event store is built (the startup log says
`mcp_http=off` instead of its bounds). `serve --mcp-stdio` works either way; `mcp connect` and
`mcp install` need HTTP, and say so when it is off. `GET /v1/me/mcp`
answers `{"http":false}`, and the console's API keys section then shows no MCP address and no
command.

## Protocol versions

Over stateful HTTP the SDK negotiates up to `2025-11-25`: a client asking for `2026-07-28` gets the
version error and negotiates again (the SDK's own client does, and the tests prove it). Over stdio,
any version the SDK speaks, `2026-07-28` included. The test suite runs on both. Through `mcp
connect` a client negotiates with the server's HTTP transport, as if it were connected directly: the
bridge relays the negotiation, `server/discover` included, on a connection of its own.

## Limits

| What | Limit |
|---|---|
| Idle HTTP session | 30 minutes |
| Sessions per key | 16 open; opening one more closes the one the key used least recently (its client gets `404` and opens another) |
| Calls in flight per key | 16 (tools and resource reads, across sessions and transports); the 17th is `rate_limited`, retry in 1 s |
| JSON-RPC batch (an array body) | refused (`400`): one message per request |
| Request body | 16 MiB |
| GET stream | no route timeout |
| `wait_for_new_mail` | 1 to 230 s (default 45), progress every 25 s |
| `get_message` | 64 KiB per body by default, up to 5 MiB |
| `get_attachment` | up to 1 MiB embedded |
| `search_messages` | 20 per page by default, up to 100 |
| Actions | 1 to 100 messages of one mailbox per call |
| Rate limit | the REST API's, through the same limiter: per address, and failures per key prefix |
| Keys per workspace | 20 alive; 30, 90 or 365 days |

## What is kept, and what is logged

- **Event store (stream resumption):** streamed responses are kept in memory, and only in memory,
  so a client whose connection dropped can resume with `Last-Event-ID`. At most **16 MiB** in total
  (oldest first out) and at most **5 minutes** each, even in an idle session: a timer removes them.
  A response larger than 4 MiB is delivered but not kept (resuming before it fails, and the client
  asks again). When a session ends, everything of it goes. A stream with nothing left is forgotten
  (except the session's one standalone GET stream), so a session busy for hours holds no more than
  the last minutes left; resuming a forgotten stream fails. Protocol `2026-07-28` has no
  resumption, and nothing is kept.
- **Log:** each call records the tool (or resource), the key prefix, the outcome, the duration and
  the ids it touched (account, message, folder). Never arguments or results: not the search text,
  subject, address, body, file name or attachment content. An account appears only when it has the
  shape of an account id; whatever a client passes in its place, or writes in a resource URI, does
  not. A refusal is logged without its cause, which may quote what was asked. The SDK is not given
  the logger.
- **No content** is stored in the database: a search reads the index; a body or attachment comes
  from the mail server and leaves in the response, as over REST.

## Keys in the console

The routes (`GET/POST /v1/me/apikeys`, `DELETE /v1/me/apikeys/{prefix}`), their JSON and their rules
are in [`console.md`](console.md#api-keys).
