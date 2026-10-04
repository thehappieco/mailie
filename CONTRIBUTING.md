# Contributing to Mailie

Thank you for helping. Mailie is licensed under Apache-2.0. Before an outside contribution is merged,
we will ask you to agree to the terms under which we accept it; until then, open an issue first.

## Before you start

- Open an issue describing the behaviour before a large change. Some of Mailie's invariants are not
  obvious from the code.
- Read [`docs/architecture.md`](docs/architecture.md) before your first patch. Its rules are not
  style: each one exists because the alternative loses mail, sends it twice or leaks a credential.
- Report security problems privately, as [`SECURITY.md`](SECURITY.md) describes, never in an issue.

## What belongs here

This repository is the self-hosted server: the daemon, the command line, the open console in `web/`
and their documentation. The hosted service's web app and its operations (deployment, hosting,
billing) live in a private repository. A change here must not depend on them, and the core of the
console (`web/src`) must not import from outside it.

## Checks

Run these before opening a pull request:

```sh
make check          # gofmt, vet, layout and the unit tests with -race (offline, no Node)
make web-check      # the console's typecheck, unit tests and build (Node 24)
make public-source  # the public snapshot builds from the allowlist
```

`make public-source` exports exactly what Git tracks under the allowlisted roots and files, so add
new files with `git add` first: a file there that Git does not track yet, or ignores as local,
fails the export. It also fails on a top-level entry it does not know, unless `.gitignore` ignores
it (it ignores `.claude/`, `.vscode/` and `.idea/`); ignore another local tool's directory the same
way, or move it aside.

If you touch IMAP, CONDSTORE, actions or sending, also run `make it` (Docker: Dovecot and Mailpit).
CI runs all of these, golangci-lint, govulncheck and gitleaks over the full history.

If you change a route the console reads, regenerate the contract fixtures from the handlers
(`go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`); never edit them by
hand.

## Conventions

- Fixtures are synthetic. Never commit a real message, credential, token or database dump.
- Test names are sentences that state a guarantee (`TestAReadKeyCannotSend`), not `TestFuncName`.
- Commit messages are in the imperative mood and describe the behaviour, without a type prefix.
- Code, comments, logs, errors and documentation are written in English. User-facing strings live
  in the console's translation catalogs (`web/src/ui/locales`), in every language it offers.
- Configuration is environment variables only; logs go through `slog` with fields.
