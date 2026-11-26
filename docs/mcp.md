# MCP

**A self-hosted server serves no MCP of its own** (platform decision 0065). Its REST API stays,
with the same keys: everything a tool did over MCP it does over REST ([`console.md`](console.md)),
reading, searching, waiting for new mail and acting, and sending, which MCP never offered. MCP is to
come through the hosted service, for a self-hosted server connected to it, once the hosted service
serves MCP and a server can connect; neither exists yet.

## What went

The release that took MCP out of the server removed:

| What | Now |
|---|---|
| Streamable HTTP at `/mcp` | every method on `/mcp`, `/mcp/` and below answers the API's JSON `404`, `{"code":"not_found","message":"no such endpoint"}`, with or without a web page, as an endpoint that does not exist does |
| `mailserver serve --mcp-stdio` | `serve` takes no flag: `mailserver: flag provided but not defined: -mcp-stdio`, before anything is opened |
| `mailserver mcp connect` and `mailserver mcp install` (and `internal/mcpbridge`, the relay behind `connect`) | `mailserver mcp …`, whatever follows, exits 1 with `mcp: this server serves no MCP of its own, so there is nothing to connect a client to: its REST API stays, and MCP comes through the hosted service once a server is connected to it`, before it reads a `.env` or any configuration |
| `MAIL_MCP_HTTP`, `MAIL_MCP_KEY`, `MAIL_MCP_ELICIT_SEND` | read by nothing and refused by nothing; the daemon says at start which of them still ask for MCP (below) |
| `GET /v1/me/mcp` (`{http, keys_send}`) | gone: a route that does not exist. Whether a key may send is said by each key's `sends` |
| The API's own `/.well-known/oauth-protected-resource` and `/.well-known/oauth-authorization-server`, which kept MCP clients from starting an OAuth discovery | not mounted; both still answer a `404` |
| The `mcp_http` and `mcp_key` fields of the start line's configuration, and its `mcp_event_store_*` bounds | gone |

A client still configured for an earlier release's MCP gets the `404` over HTTP, and a client that
launched `mailserver mcp connect` or `serve --mcp-stdio` gets that command's error in its log of
the server. Remove the entry from the client's configuration (Claude Code: `claude mcp remove
--scope user mailie`; Cursor: `~/.cursor/mcp.json`; Claude Desktop: its
`claude_desktop_config.json`; `mcp install` kept the file as it was before it first changed it in
`<file>.bak-mailie`), and revoke the key it held (`mailserver apikey revoke PREFIX`), unless a
script uses it over REST.

## The warning at start

Nothing refuses the retired variables, so an environment written for an earlier release still
starts. Where one of them asks for MCP, the daemon says so once at start, at warn, naming the
variables and never a value (`MAIL_MCP_KEY` held a key):

```
time=… level=WARN msg="this server serves no MCP of its own, at /mcp or over standard input and output: MCP comes through the hosted service once a server is connected to it; these variables ask for MCP and do nothing" variables="[MAIL_MCP_HTTP MAIL_MCP_KEY]"
```

- `MAIL_MCP_KEY` asks for MCP whenever it is set, whatever its value.
- `MAIL_MCP_HTTP` and `MAIL_MCP_ELICIT_SEND` ask for it when set to anything that is not a false
  boolean (`true`, or a value that is no boolean at all). `MAIL_MCP_HTTP=false` asks for nothing,
  and says what this server does anyway: it is no warning.
- An empty value is unset, as every variable is. The variables are listed in that order, as
  `config.Config.RetiredMCP` holds them.

Remove them from the environment (`.env`, `deploy/.env`, `/etc/mailie/mailie.env`) and the warning
goes. A release from before this one reads them again: going back to it serves `/mcp` unless
`MAIL_MCP_HTTP=false`.

## The tools stay, as a library

`internal/mcp` is kept, with its tests, and nothing in this repository mounts it: no binary built
here links it (`go list -deps ./cmd/mailserver` names neither it nor the go-sdk). It is the tool
layer over `internal/service`, kept so that a later binary can serve MCP over the same service:

- `mcp.New(svc, logger, version)` builds the server; `(*Server).HTTPHandler` serves stateful
  Streamable HTTP (one `sdk.Server` per session, bound to the key that opened it, with an event
  store of its own bounded in bytes and age) and `(*Server).RunStdio` one session over standard
  input and output.
- Its tools are `list_accounts`, `list_folders`, `search_messages`, `get_message`,
  `get_attachment` and `wait_for_new_mail` to read, and `mark_read`, `flag_message`,
  `move_message` and `trash_message` to act (the trash a tool of its own, the destructive one), with
  `mail://` resources and inbox subscriptions; there is no send tool.
- It is an adapter, as the REST API is: every tool calls one service method with the caller's
  principal, and every rule about who sees what, a key's scope, actions and what reaches the mail
  server is decided there. `Service.AuthenticateTool` still decides what a tool's credential
  reaches (API keys only, never a session), and what a tool logs is ids, never an argument or a
  result.
- Its tests run as before (`go test ./internal/mcp`), in memory, against the real service.

Whether and how the hosted service serves MCP for a connected server, under which key terms, and
with which of these tools, is decided there, not here.
