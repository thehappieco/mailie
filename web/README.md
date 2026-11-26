# Mailie console core

Vue 3 + strict TypeScript, with no router, no Pinia and no UI library: the same shape as the
console of Wappie, a sibling product, whose tokens, class names, `ConsoleDialog`, `AppearanceMenu`,
`PasswordInput` and generation-counter stores it copies. The architecture and the HTTP contract are
in `../docs/console.md`.

This project is the **core** the console's editions build on: signing in, the session and its
vault, the OAuth return, workspaces and the switcher between them, mailboxes and connecting them,
their keys and grants, their sync, API keys, the person's permissions, the event stream and the
frame (`ConsoleShell`, `AppOverlays`, `useConsoleFrame`, `useAppLifecycle`). An edition configures
the core before mounting (`src/edition.ts`) and compiles it from source through an alias of its
own, in a project of its own, which builds and serves the page; nothing here imports one.
`../docs/console.md`, "Editions", lists what an edition gives the core and the other extension
points.

There is no edition here and nothing to build or serve. A self-hosted server has no web console
(platform decision 0063): it is run from the command line and used through REST and MCP with keys.
The open console that this project used to build (`src/main.ts`, `src/open`, `index.html`, its
`App.vue` and the settings panels only it drew) was deleted; the git history keeps it.

## Check

```sh
npm ci
npm run typecheck  # vue-tsc, including the specs and vitest.config.ts
npm test           # vitest in Node, with no browser or real DOM
```

`make web-check` in the repository's root runs both.

The specs run in two Vitest projects (`vitest.config.ts`), both under the specs' own edition
(`test/edition.ts`, configured by `test/setup.ts`): placeholder texts nobody agrees to, under the
daemon's default revisions (`internal/config/consent.go`), and a test console built on the core's
frame. `*.page.spec.ts` run in `page`: they mount components on a small fake page (`test/dom.ts`,
no new dependency) for what only a click shows. A spec that loads the modules again, as a new page
load would, configures the new ones the same way (`freshModules` in `test/support.ts`).
`test/support.ts`, `test/dom.ts`, `test/mount.ts`, `test/client.env.ts`, `test/accountServer.ts`,
`test/palette.ts` and `test/i18nGuard.ts` are also what an edition's specs build on.

`test/i18nGuard.ts` is the translation guard every project runs: every literal `t('…')` translated
in all four languages with the same placeholders, each key in exactly one catalog, no unused key in
the project's own catalogs. The core's catalogs (`src/ui/locales`) hold only the core's own words:
a key an edition still uses once the core no longer does lives in that edition's catalogs.

`test/editions.spec.ts` holds the core to never importing from outside `src/` or through an alias (a
scoped name is allowed only when it is one of `package.json`'s `dependencies`), to calling its frame
nothing of its own (each edition names it), and every file under `src/` and `public/` to naming
none of the hosted service's names (`test/hosted.ts`: the company, the hosted addresses, the policy
and terms in each language, the cloud's `2026-09-` revisions). The one spelling of the company it
sets aside is the quoted package name in an import of the kit, `@thehappieco/kit`, which is code
and never reaches a screen.

`src/crypto/mailie.ts` is Mailie's profile of the kit's key scheme (`../docs/key-scheme.md`): the
kit's own since 0.7.0 (`@thehappieco/kit/profiles/mailie`), re-exported with the few helpers the
kit leaves to the console, which the ceremonies use through `src/crypto/account.ts`, and mailbox
keys and their grants through `src/crypto/mailbox.ts` (made, sealed and opened in the browser
only; `../docs/console.md`, "Mailbox keys").
`test/keyscheme.spec.ts` holds it to the golden vectors the Go side writes
(`../internal/keyscheme/testdata`, `go test ./internal/keyscheme -run
TestTheVectorsAreWhatTheProfileWrites -update`), including every case that must fail; as with the
contract fixtures, a missing file is a failure, never something to create from here. The kit is
installed from its release tarball, pinned by integrity in `package-lock.json`.

`test/contract.spec.ts` reads `../internal/api/testdata/contract/*.json`, written by the Go side
(`go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`), and holds the
specs' edition's revisions to the daemon's defaults the fixtures are written under. A missing
fixture is a failure, never something to create from here. The fixtures of routes the core never
calls (messages, actions on them, sending) are checked by the edition that reads them.

## Browser QA helpers (outside `npm test`)

`test/browser/fakeDaemon.mjs` is the daemon as far as the core can tell, in memory, for an
edition's browser QA: the core's routes (users and sessions, the key scheme's ceremonies with the
people it seeds enrolled under their password, or never enrolled with `notEnrolled`, their
enrolment made by `test/browser/keyScheme.mjs`, mailboxes and their OAuth flows, links at the
provider (`gmailFlows` offers Gmail the web flow), sync and the event stream, actions consent, a
workspace's API keys and the ones a person created, `GET /v1/me/mcp`, storage) and the helpers the
QA scripts share. An edition's QA imports it and adds its own routes and state through `extend`,
so a change to the core's contract is made once. Unless asked to list each person's personal
workspace (`personal`), it answers `GET /v1/workspaces` with `404`, as a server older than them
would, and every list is the person's whole.

## Provisional brand

There is no official Mailie brand kit yet. `src/brand/mark.ts` and `public/favicon-*.svg` are
a provisional mark built with the family's rules (frame W=12u, I=3πu, R=πu; envelope 6u by W/e; flap
at t/2 to the golden section; colour `#B24B1E` = round(18π², 24π, 11e)). When the official kit exists,
replace the literals in those files, in `mark.css` and in `BrandLockup.vue`. An edition takes the
favicons from `public/` and may add a byline under the wordmark (`byline`).
