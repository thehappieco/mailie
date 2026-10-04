# MCP

The daemon speaks the Model Context Protocol, so an AI assistant (Claude Code, for one) or any other
MCP client can search and read a person's mailboxes and, with a write key, act on them. It is the
same service as the REST API: the same rules about who sees what, the same search of the index, the
same bodies fetched from the mail server on request and never stored, the same actions behind the
same consent. No authorization rule lives in `internal/mcp`.

- **Endpoint:** `/mcp` on the daemon's listener (`http://127.0.0.1:8080/mcp` by default, or
  `https://<your host>/mcp` behind your proxy): Streamable HTTP, **stateful** (POST, GET and
  DELETE). On unless `MAIL_MCP_HTTP=false`.
- **stdio:** `mailserver serve --mcp-stdio`, for a client that launches the daemon itself.
- **Credential:** an API key in `Authorization: Bearer <key>`. There is no OAuth.

## Who gets in, and what they see

- **Keys only.** `/mcp` refuses a console session token (`401`): a session is a person in a
  browser, and copying it into an assistant's configuration must not work.
- **A person's key.** A tool reaches a person's mailboxes only with a key that person created in
  the console (**API keys & MCP**), for the mailboxes, scope and lifetime they chose. Creating it is
  their agreement to what a tool holding it can do, and the key records the revision of the text
  they saw (`terms_version`). A key an administrator issued for a person, with no such agreement, is
  refused here and over REST (`403`).
- **Instance keys** (the operator's, from `mailserver apikey create`) reach only the mailboxes
  nobody owns. Over REST they keep seeing every mailbox.
- **Scopes:** `read` searches and reads; `write` also marks read, stars, moves, archives and
  trashes. Actions still need the mailbox owner's permission for actions in the console, checked
  before every command that changes the mailbox, as over REST.
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
| `list_accounts` | the mailboxes the key reaches: id, address, provider, state, sync, which actions are available | read |
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

`resources/subscribe` accepts only `mail://{account}/folder/inbox` of a mailbox the key sees, and
sends `notifications/resources/updated` when new mail reaches that inbox. It is computed with the
session's own key, so it never speaks of another mailbox, and it stops when the key stops working:
the key is checked before each wait on the journal and again before announcing what arrived during
it. Few clients act on that notification today; `wait_for_new_mail` is what hands new mail to a
model.

Every result carries `cacheScope: "private"` and `ttlMs: 0`. The protocol's default, `"public"`,
would let an intermediary keep a response and serve it to someone else, and this is one key's mail.

## Connecting a client

**Claude Code:**

```sh
claude mcp add --transport http mailie http://localhost:8080/mcp \
  --header "Authorization: Bearer <your key>"
```

The console's **API keys & MCP** section shows this command with the server's own address, and a
new key's dialog copies it with the key in it, whenever the server answers MCP over HTTP.

**Any other client** that sends a fixed `Authorization: Bearer` header works the same way.

**claude.ai web connectors** and Claude Desktop's remote connectors need OAuth, which Mailie does not
offer yet.

**stdio**, for a client that launches the daemon, such as Claude Desktop's
`claude_desktop_config.json`:

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
when the daemon starts, and the daemon exits when the client hangs up.

**Switching HTTP off.** `MAIL_MCP_HTTP=false` (default `true`) leaves `/mcp` unmounted: every method
on `/mcp` and under it answers the API's JSON `404` (`{"code":"not_found","message":"no such
endpoint"}`), with or without a console, and no event store is built (the startup log says
`mcp_http=off` instead of its bounds). `serve --mcp-stdio` works either way. `GET /v1/me/mcp`
answers `{"http":false}`, and the console's API keys section then shows no MCP address and no
command.

## Protocol versions

Over stateful HTTP the SDK negotiates up to `2025-11-25`: a client asking for `2026-07-28` gets the
version error and negotiates again (the SDK's own client does, and the tests prove it). Over stdio,
any version the SDK speaks, `2026-07-28` included. The test suite runs on both.

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
| Keys per person | 20 alive; 30, 90 or 365 days |

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
