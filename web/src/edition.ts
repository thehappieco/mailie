// What an edition of the console brings to the core.
//
// The core (everything under src/ but open/) is the console every
// edition shares: signing in, the session, connecting mailboxes and their
// sync, API keys, the person's account and permissions, the event stream.
// An edition is the rest: the console shown once someone is signed in, with
// its own sections, and the texts a person agrees to, which belong to
// whoever runs the server. The open edition (src/open, served by a
// self-hosted server) is one; another builds on this core from its own
// project, through an alias. Each configures one before mounting.
//
// A text a person agrees to travels with its revision. The core sends the
// revision of the text it showed, never the one the server names: when the
// server names another, the page offers a reload instead of an agreement.

import type { Component } from 'vue'
import type { Account } from './api/types'
import { addCatalogs, setOfferedLocales, type Catalog, type Locale } from './ui/i18n'
import { setPreferenceStore, type PreferenceStore } from './ui/preferences'

/** A text a person agrees to, and the revision the server knows it by (MAIL_CONSENT_VERSION_*). */
export interface AgreementText {
  version: string
  /** The text itself. Its words are what the person agrees to: change them only with a new version. */
  component: Component
}

/**
 * What a step-up is asked for (docs/key-scheme.md section 11; state/stepUp.ts):
 * linking a mailbox ('link'), giving Read with a grant ('grant'), or writing
 * or handing on a mailbox's key ('key').
 */
export type StepUpReason = 'link' | 'grant' | 'key'

/** Where the frame draws the brand: atop the sidebar ('menu'), and atop the signed-out card ('card'). */
export type BrandPlace = 'menu' | 'card'

/** A permission's text, and what the core says when it is not the one the server asks about. */
export interface PermissionText extends AgreementText {
  /** The server asks about another revision than this page carries: it changed while the page was open. */
  changedWhileOpen(): string
  /** The person agreed to an older revision, on date (already formatted). */
  changedSince(date: string): string
}

export interface Edition {
  /** What a signed-in person sees: the edition's sections, built on components/ConsoleShell.vue. */
  console: Component
  /** What sync stores: agreed to before anything from a mailbox is kept. */
  sync: PermissionText
  /** What actions on messages change: agreed to before any is made. */
  actions: PermissionText
  /**
   * What a tool holding a new API key can do: agreed to by creating the key.
   * Takes `write` (the scope acts), `send` (the scope sends) and `team` (the
   * name of the team the key is created in; empty in a personal workspace)
   * props.
   */
  keyTerms: AgreementText
  /** What the frame (components/ConsoleShell.vue) calls what it holds. Translated, or a name, which never is. */
  shell: {
    /** The root of the header's breadcrumb, before the section shown, and the lockup's name beside the product's. */
    rootLabel(): string
    /** The accessible name of the sidebar, which holds the sections. */
    navLabel(): string
  }
  /** Sentences of core screens whose words depend on the edition. */
  copy: {
    /** The section with the person's own settings and permissions, as other sentences name it. */
    accountSection(): string
    /** Under an empty list of mailboxes: what connecting one does. */
    mailboxesIntro(): string
    /** In a syncing mailbox's sheet: what the index keeps, and where a message's body comes from. */
    indexHint(): string
    /** In the dialog that turns actions off: what stops. */
    actionsOff(): string
    /**
     * Beside a write that needs a step-up (giving Read with a grant, a
     * mailbox's key): when the person is asked to prove themselves again.
     * Unset: the core's sentence, which names the password the core's
     * step-up asks for. An edition that sets stepUp words it its own way.
     */
    stepUpHint?(): string
    /**
     * Beside a key that stopped because its creator's account was recovered
     * (stop_reason account_recovered): why, in a sentence. mine: the person
     * reading created it. Unset: the core's sentence, which says only that
     * the account was recovered. An edition whose people recover their
     * account elsewhere names where.
     */
    accountRecovered?(mine: boolean): string
  }
  /**
   * Whether the API keys section shows how to reach the MCP server at this
   * page's origin + /mcp: the URL, and the Claude Code command. Shown only
   * where the server also says it answers there (GET /v1/me/mcp, state/mcp.ts),
   * which an edition that sets this false never asks.
   */
  mcp: boolean
  /**
   * Whether people make and run teams here (the server's local workspace
   * source): creating a team, inviting into it and administering its
   * members, and accepting a team invitation while signed in. An edition
   * whose workspaces come from elsewhere leaves it out: an invitation link
   * then only ever creates an account. Mailboxes, their grants, their sync
   * and a workspace's API keys are the core's either way.
   */
  teams?: boolean
  /**
   * Whether the account section names the person's role on this server (an
   * owner, who brings people onto it, or a member): a self-hosted server's.
   * An edition whose people have no role on the server leaves it out.
   */
  serverRole?: boolean
  /**
   * The edition's own sign-in, in place of the core's password card on the
   * signed-out screen (components/SignInView.vue): for people who sign in
   * another way, through a route of the edition's own whose reply is what
   * POST /v1/auth/login answers. It hands that reply to adoptSession
   * (state/session.ts), which stores the session and loads the person as the
   * password sign-in does. The core keeps the frame around it: the brand and
   * the appearance menu above, the notes about a session that ended or a
   * mailbox waiting to finish connecting, and legal.signInFooter below. An
   * invitation link signs nobody up through it. Unset: the core's card.
   *
   * A sign-in that opened or made the person's account key keeps it first
   * (keepAccountKey, state/accountVault.ts), then adopts the session, which
   * begins keyed when this browser holds the key of the person it names; one
   * that never adopts its session settles the key it kept
   * (settleRecord(holder, false)).
   */
  signIn?: Component
  /**
   * The edition's own step-up (docs/key-scheme.md section 11), in place of
   * the core's password dialog (components/StepUpDialog.vue): for people who
   * prove themselves again another way, such as signing in again at their
   * identity provider. state/stepUp.ts calls it when a flow needs a fresh
   * step-up, once for every flow waiting then, with what the first of them
   * asked it for. It resolves once this session's step-up time is fresh,
   * which the edition records with steppedUp (state/session.ts) from what its
   * route answered, and the flows go on; it rejects with StepUpCancelled
   * (state/stepUp.ts) when the person gives up, which stops them without a
   * word, and with any other error to stop them with that one. It may be
   * called after awaits, out of the click that started the flow, so a window
   * it opens needs a click of its own. Unset: the password dialog.
   */
  stepUp?: (reason: StepUpReason) => Promise<void>
  /**
   * Called once the person has signed out on purpose (from the frame, the
   * account section, or everywhere), after the core has ended the session in
   * this browser and told the server; never when a session expires, is
   * refused, or ends in another tab. An edition may navigate away from here.
   */
  signedOut?: () => void
  /** The line under the wordmark ("by …"), if any. A name, never translated. */
  byline?: string
  /**
   * The edition's own look in the places the frame keeps for it: the
   * sidebar's top and the header (components/ConsoleShell.vue), and the top
   * of the signed-out card (components/SignInView.vue). Each one set is drawn
   * instead of the core's there; unset, the core's. Colours and corners are
   * not set here: they are the tokens of ui/palette.css, which an edition's
   * entry may declare its own way instead of importing that file.
   */
  look?: {
    /**
     * The brand, in place of the core's lockup (components/BrandLockup.vue).
     * Takes `place` (BrandPlace) and `label`: the accessible name the core
     * gives its lockup there, which the brand carries as its own.
     */
    brand?: Component
    /**
     * In place of the core's appearance menu (components/AppearanceMenu.vue):
     * what shows and changes the theme (ui/preferences.ts) and the language
     * (ui/i18n.ts), among the languages offered. One root element, no props.
     */
    appearance?: Component
  }
  /**
   * The languages offered, among the core's (ui/i18n.ts): the only ones a
   * person may choose, and a saved or browser language outside them is passed
   * over for the next, then English. English is always among them, and
   * configureEdition refuses a list without it or with a language the core
   * does not have. Unset: all of the core's. Every catalog still carries the
   * four translations.
   */
  locales?: readonly Locale[]
  /**
   * Where the theme and the language a person chooses are kept, and read back
   * from (ui/preferences.ts, PreferenceStore): before the first frame, whenever
   * the page restores them, and at every choice. Unset: the core's own
   * (hostPreferences), a cookie of this host and local storage. Installed by
   * configureEdition, so before the page chooses either.
   */
  preferences?: PreferenceStore
  /** Where the edition's legal texts go. Nothing is drawn in a place it leaves empty. */
  legal?: {
    /** Below the sign-up form: what creating the account agrees to. */
    signUp?: Component
    /** At the foot of the sign-in card. */
    signInFooter?: Component
    /** In the form that connects a mailbox, before any access is granted. */
    accountAccess?: Component
  }
  /**
   * Where who uses a mailbox, and what each may do, is managed, when that is
   * not on the mailbox's sheet (components/AccountSheet.vue): the sheet then
   * draws one link there in place of its Access section (AccessPanel), for
   * the same people it would show that section to (a team's owners and
   * admins). The mailbox's key (MailboxKeyPanel), the person's own, stays on
   * the sheet. Unset: the sheet holds the Access section.
   */
  mailboxAccess?: {
    /** The link's target for this mailbox: a path of this origin. */
    href(account: Account): string
    /** The link's words. Translated. */
    label(): string
  }
  /** Translations beyond the core's, for the edition's own sources. */
  catalogs?: Catalog[]
  /**
   * How long this browser keeps the person's account key (docs/key-scheme.md
   * section 7). Unset: a self-hosted server's rule, wiped at sign-out and
   * whenever the page finds no valid session, since every sign-in opens the
   * password wrap anyway. outlivesExpiry keeps it past a session that merely
   * expires (the hosted service's rule, whose next sign-in then needs no
   * product key): one a page of the console established as expired by the
   * server's clock, at its expiry timer or by a refusal the server's answer
   * dates past expires_at, which marks the record until a session begins
   * with it again. Everything else wipes it: a refusal before the expiry
   * (revoked, ended by a password change, a recovery or a reset, its person
   * disabled), a session given up on unchecked, no session record beside an
   * unmarked key, and sign-out, in every edition (state/session.ts).
   */
  accountKey?: { outlivesExpiry?: boolean }
}

let configured: Edition | null = null

/**
 * Sets the edition. Call before mounting, and before anything renders or asks
 * the server, or chooses the language (initializeLocale). An edition whose
 * languages it refuses (locales) throws, and changes nothing. An edition may
 * set itself again before mounting, once the page knows more (how people sign
 * in, say), after the language was chosen: a language shown that the new one
 * does not offer then gives way as initializeLocale chooses, saving nothing.
 */
export function configureEdition(value: Edition): void {
  // The edition's store first: a language it no longer offers gives way to the one its store keeps.
  const before = setPreferenceStore(value.preferences ?? null)
  try { setOfferedLocales(value.locales ?? null) } catch (error) { setPreferenceStore(before); throw error }
  configured = value
  if (value.catalogs) addCatalogs(...value.catalogs)
}

/** The configured edition. Without one, a screen would be missing its text: that is a bug, not a state. */
export function edition(): Edition {
  if (!configured) throw new Error('edition: none configured; call configureEdition before mounting')
  return configured
}
