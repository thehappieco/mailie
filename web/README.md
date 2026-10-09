# Mailie console

Vue 3 + strict TypeScript + Vite, with no router, no Pinia and no UI library: the same shape as the
console of Wappie, a sibling product, whose tokens, class names, `ConsoleDialog`, `AppearanceMenu`,
`PasswordInput` and generation-counter stores it copies. The architecture and the HTTP contract are
in `../docs/console.md`, and the OAuth client registrations in `../README.md`.

This project is the **open console**, what a self-hosted server serves (`src/main.ts`,
`src/open`): Mailboxes, Members (a team's people and invitations, or making a team), API keys & MCP,
Storage and Account. It is also the **core** other editions build on: signing in, the session and
its vault, the OAuth return, workspaces and the switcher between them, mailboxes with their grants,
access and sync, API keys, the account section and its permissions, Storage and the event stream. An edition configures
the core before mounting (`src/edition.ts`) and compiles it from source through an alias of its
own; nothing here imports one. `../docs/console.md`, "Editions", lists what an edition gives the
core and the other extension points.

## Run

```sh
npm ci
npm run dev        # http://localhost:5174; /v1 and /mcp go to MAIL_DEV_TARGET (default http://127.0.0.1:8080)
```

The port is fixed (`strictPort`): the registered Google OAuth web client redirects to
`http://localhost:5174/oauth/return`, port included. Another edition's dev server may take the same
port for the same reason, so run one at a time. Use `localhost`, not `127.0.0.1`, in the browser.

## Check

```sh
npm run typecheck  # vue-tsc, including the specs and vite.config.ts
npm test           # vitest in Node, with no browser or real DOM
npm run build      # typecheck + build into dist/, which the daemon serves from MAIL_WEB_DIR
```

The specs run in two Vitest projects (`vite.config.ts`), both with the open edition configured
(`test/setup.ts`). `*.page.spec.ts` run in `page`: they mount components on a small fake page
(`test/dom.ts`, no new dependency) for what only a click shows: a dialog that stays open on a
failure, a switch that asks before turning off, a key shown once and forgotten when its dialog
closes. A spec that loads the modules again, as a new page load would, configures the new ones the
same way (`freshModules` in `test/support.ts`). `test/support.ts`, `test/dom.ts`, `test/mount.ts`,
`test/client.env.ts` and `test/i18nGuard.ts` are also what another edition's specs build on.

`test/i18nGuard.ts` is the translation guard every edition runs: every literal `t('…')` translated
in all four languages with the same placeholders, each key in exactly one catalog, no unused key in
the project's own catalogs. `test/editions.spec.ts` holds the core to never importing from outside
`src/` or through an alias (a scoped name is allowed only when it is one of `package.json`'s
`dependencies`), the open texts to naming no company and linking no policy, and every
file under `src/` and `public/`, and `index.html`, to naming none of the hosted service's names
(`test/hosted.ts`: the company, the hosted addresses, the policy and terms in each language, the
cloud's `2026-09-` revisions). The one spelling of the company it sets aside is the quoted package
name in an import of the kit, `@thehappieco/kit`, which is code and never reaches a screen.
`npm run build` fails on an output file that has one (`vite.config.ts`), whatever brought it in.

`src/crypto/mailie.ts` is Mailie's profile of the kit's key scheme (`../docs/key-scheme.md`), not
yet called by any screen. `test/keyscheme.spec.ts` holds it to the golden vectors the Go side writes
(`../internal/keyscheme/testdata`, `go test ./internal/keyscheme -run
TestTheVectorsAreWhatTheProfileWrites -update`), including every case that must fail; as with the
contract fixtures, a missing file is a failure, never something to create from here. The kit is
installed from its release tarball, pinned by integrity in `package-lock.json`.

`test/contract.spec.ts` reads `../internal/api/testdata/contract/*.json`, written by the Go side
(`go test ./internal/api -run TestTheContractFixturesMatchTheHandlers -update`), and holds the open
console's text revisions (`src/open/versions.ts`) to the daemon's defaults. A missing fixture is a
failure, never something to create from here. The fixtures of routes the open console never calls
(messages, actions on them, sending) are checked by the edition that reads them.

## Browser QA (optional, outside `npm test`)

`test/browser/console.mjs` opens the open console in a real Chromium with every `/v1` call answered
by an in-memory fake daemon and the Google and Microsoft pages intercepted. It walks sign-in and
sign-up by invitation with the key scheme (no password ever in a request; the recovery code shown
once; a password change; the step-up before replacing the recovery code), the one-time upgrade of
an account made before it (and its refusal for an address that enrolled here), recovery with the
code and a reset link, an IMAP mailbox, iCloud (a refused password, an app-specific one, a custom
domain), the loopback and web OAuth flows, a Gmail consent that came back without the mailbox, a
folder listing the mail server refuses, removal (with the mailbox's id repeated in `confirm`, as the
daemon requires), the account section and an expired session, on desktop and phone, light and dark,
then in pt-BR and German; then sync (the consent card, the first sync over the event stream with
`Last-Event-ID` on every reconnection, Sync now, folders from the index, turning it off, the text
changing while the page is open, a revoked session), actions in the Account section (a consent to
an older text is paused), the API keys of the personal workspace and the MCP endpoint at this
origin (each scope; what a key holds ticked mailbox by mailbox; the key shown once, copied, never
kept; a mailbox given to it from its sheet, its sends; revoked, and listed among the keys the
person created in Account), API keys on a server with MCP over HTTP off (no address, no command)
and on one whose keys do not send (no send scope), and Storage (a mailbox whose account needs
authorizing again keeps its index; turning sync off reads the figures again). It fails on a page error, a CSP
violation, horizontal scrolling, any legal link, byline or company name, and any request to the
message or sending routes, which its fake daemon does not implement.
`QA_ONLY=console|keys-scheme|sync|actions|keys|storage` runs one group.

The fake daemon is `test/browser/fakeDaemon.mjs`: the core's routes (users and sessions, the key
scheme's ceremonies with the people it seeds enrolled under their password, or not upgraded yet
with `notUpgraded`, their enrolment made by `test/browser/keyScheme.mjs`, mailboxes
and their OAuth flows, sync and the event stream, actions consent, a workspace's API keys and the
ones a person created, `GET /v1/me/mcp`, storage) and the helpers the QA scripts share. Another
edition's QA imports it and adds its own routes and state through `extend` (the cloud app's adds
Mail and sending), so a change to the core's contract is made once. Unless asked to list each
person's personal workspace (`personal`, which the keys pass sets, as a server with workspaces
does), it answers `GET /v1/workspaces` with `404`, as a server older than them would, and every
list is the person's whole.

`test/browser/teams.mjs` adds workspaces through the same `extend`: Ana's personal workspace, a team
she owns whose mailboxes are the team's (one Bea linked and synced for the team, which Ana reads
and sends from and Carol only sends from; one only Ana reads, with sync off; one only Bea reads,
under the agreement the upgrade carried over from her, which Carol manages and Ana holds nothing
on), and a second team whose invitation waits for her. It walks the switcher; every mailbox of the
team as a card its owner manages by her role, one she does not read without its folders; a
mailbox's access ticked and saved (act ticks read; a change that also adds sets the whole grant; one
that only takes away names the flags taken), the last reader's Read kept and what she cannot give
said beforehand; a team mailbox's sync turned on for the team after its text, and the carried-over
agreement confirmed; the team's people (the last reader marked, a role changed and back, an
invitation's link shown once, kept through Escape, copied and gone with its dialog); creating a
team; turning her own sync off, which reaches her personal workspace's mailboxes; an invitation
opened signed in; the team's API keys (a key given Read on the mailbox Ana reads and Send on one
she does not, where she cannot give Read; the key in that mailbox's access, taken out of it there);
and then Carol, a member: her cards without folders, messages, access, sync switch or removal, a
line saying who manages the team's people, access and API keys, and no Members or API keys. On desktop and phone,
light and dark, then in Portuguese and German, with the words each language expects read from the
console's catalogs. It takes the same `QA_*` variables as `console.mjs`.

```sh
npm run build && npx vite preview --port 4174 &     # or npm run dev, with QA_ORIGIN=http://localhost:5174
QA_ORIGIN=http://localhost:4174 \
QA_PLAYWRIGHT_MODULE=/path/to/node_modules/playwright/index.mjs \
QA_SCREENSHOTS=/tmp/mailie-screens \
node test/browser/console.mjs
```

The CSP is the same in development and in the build, and the same as the header the daemon sends
for every edition (`internal/webui`): `style-src` allows inline style, which an edition that shows a
message's HTML in a sandboxed `srcdoc` frame needs, and Vite's dev server too; script is only ever
`'self'`.

## Provisional brand

There is no official Mailie brand kit yet. `src/brand/mark.ts` and `public/favicon-*.svg` are
a provisional mark built with the family's rules (frame W=12u, I=3πu, R=πu; envelope 6u by W/e; flap
at t/2 to the golden section; colour `#B24B1E` = round(18π², 24π, 11e)). When the official kit exists,
replace the literals in those files, in `mark.css` and in `BrandLockup.vue`. The open console shows
the wordmark alone; an edition may add a byline under it (`byline`).
