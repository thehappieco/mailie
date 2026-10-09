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
import { addCatalogs, type Catalog } from './ui/i18n'
import { setSharedDomain } from './ui/preferences'

/** A text a person agrees to, and the revision the server knows it by (MAIL_CONSENT_VERSION_*). */
export interface AgreementText {
  version: string
  /** The text itself. Its words are what the person agrees to: change them only with a new version. */
  component: Component
}

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
   */
  signIn?: Component
  /**
   * Called once the person has signed out on purpose (from the frame, the
   * account section, or everywhere), after the core has ended the session in
   * this browser and told the server; never when a session expires, is
   * refused, or ends in another tab. An edition may navigate away from here.
   */
  signedOut?: () => void
  /** The line under the wordmark ("by …"), if any. A name, never translated. */
  byline?: string
  /** A domain whose hosts share the theme and language preferences (a cookie Domain). Unset: this host only. */
  cookieDomain?: string
  /** Where the edition's legal texts go. Nothing is drawn in a place it leaves empty. */
  legal?: {
    /** Below the sign-up form: what creating the account agrees to. */
    signUp?: Component
    /** At the foot of the sign-in card. */
    signInFooter?: Component
    /** In the form that connects a mailbox, before any access is granted. */
    accountAccess?: Component
  }
  /** Translations beyond the core's, for the edition's own sources. */
  catalogs?: Catalog[]
  /**
   * How long this browser keeps the person's account key (docs/key-scheme.md
   * section 7). Unset: a self-hosted server's rule, wiped at sign-out and
   * whenever the page finds no valid session, since every sign-in opens the
   * password wrap anyway. outlivesSession keeps it past a session that
   * merely ends (the hosted service's, whose next sign-in then needs no
   * product key); sign-out wipes it in every edition.
   */
  accountKey?: { outlivesSession?: boolean }
}

let configured: Edition | null = null

/** Sets the edition. Call before mounting, and before anything renders or asks the server. */
export function configureEdition(value: Edition): void {
  configured = value
  setSharedDomain(value.cookieDomain ?? '')
  if (value.catalogs) addCatalogs(...value.catalogs)
}

/** The configured edition. Without one, a screen would be missing its text: that is a bug, not a state. */
export function edition(): Edition {
  if (!configured) throw new Error('edition: none configured; call configureEdition before mounting')
  return configured
}
