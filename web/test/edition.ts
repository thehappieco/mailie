// The edition the core's specs run under. The core draws nothing before an
// edition is configured (src/edition.ts), and no edition ships with it: a
// self-hosted server has no web console (platform decision 0063), and the
// editions built on this core live in their own projects. This one is for
// the specs alone, and nothing under src/ imports it.
//
// Its texts are placeholders that nobody agrees to. Its revisions are the
// daemon's defaults (internal/config/consent.go), the ones the handlers write
// the contract fixtures under (internal/api/testdata/contract), so what the
// core sends in the specs is what a daemon left at its defaults accepts.
// Its words are plain strings, never t(): the core's catalogs hold only the
// core's own words (test/i18n.spec.ts).
import { defineComponent, h } from 'vue'
import ConsoleShell, { type ConsoleSection } from '../src/components/ConsoleShell.vue'
import type { Edition } from '../src/edition'

/** The daemon's default revision of the sync text (config.DefaultSyncConsentVersion). */
export const SYNC_TEXT_VERSION = '2026-10-open-sync-4'
/** The daemon's default revision of the actions text (config.DefaultActionsConsentVersion). */
export const ACTIONS_TEXT_VERSION = '2026-10-open-actions-3'
/** The daemon's default revision of the key terms (config.DefaultKeyTermsVersion). */
export const KEY_TERMS_VERSION = '2026-10-open-api-keys-3'
/** The daemon's default revision of the sending text (config.DefaultSendConsentVersion), which the core never shows. */
export const SEND_TEXT_VERSION = '2026-10-open-sending'

/** What sync stores, as a placeholder: names the team it is agreed for, when there is one. */
export const SyncText = defineComponent({
  name: 'SyncText',
  props: { team: { type: String, default: '' } },
  setup: props => () => h('p', { class: 'test-sync-text' }, props.team ? `The test edition’s sync text, agreed for ${props.team}.` : 'The test edition’s sync text.'),
})

/** What actions change, as a placeholder. */
export const ActionsText = defineComponent({
  name: 'ActionsText',
  setup: () => () => h('p', { class: 'test-actions-text' }, 'The test edition’s actions text.'),
})

/** What a tool holding a new key can do, as a placeholder that says what it was told. */
export const KeyTermsText = defineComponent({
  name: 'KeyTermsText',
  props: { write: Boolean, send: Boolean, team: { type: String, default: '' } },
  setup: props => () => h('p', { class: 'test-key-terms' }, `The test edition’s key terms (write: ${props.write}, send: ${props.send}, team: ${props.team || 'none'}).`),
})

/** A section of the test console: a heading's worth of words. */
const Placeholder = defineComponent({ name: 'Placeholder', setup: () => () => h('p', { class: 'test-section' }, 'A section of the test console.') })

/** The sections of the test console: one of the workspace shown, and the person's own. */
export const testSections: ConsoleSection[] = [
  { id: 'mailboxes', label: 'Mailboxes', description: 'The mailboxes of the workspace shown.', icon: 'mail', component: Placeholder },
  { id: 'account', label: 'Account', description: 'The person’s own settings.', icon: 'user', component: Placeholder, scope: 'person' },
]

/** The test console: the core's frame around the test sections. */
export const TestConsole = defineComponent({
  name: 'TestConsole',
  setup: () => () => h(ConsoleShell, { sections: testSections, home: 'mailboxes', account: 'account' }),
})

export const testEdition: Edition = {
  console: TestConsole,
  sync: {
    version: SYNC_TEXT_VERSION,
    component: SyncText,
    changedWhileOpen: () => 'The sync text changed while this page was open.',
    changedSince: () => 'The sync text changed since you turned sync on.',
  },
  actions: {
    version: ACTIONS_TEXT_VERSION,
    component: ActionsText,
    changedWhileOpen: () => 'The actions text changed while this page was open.',
    changedSince: date => `The actions text changed since you allowed them on ${date}.`,
  },
  keyTerms: { version: KEY_TERMS_VERSION, component: KeyTermsText },
  shell: {
    rootLabel: () => 'Console',
    navLabel: () => 'Console navigation',
  },
  copy: {
    accountSection: () => 'Account',
    mailboxesIntro: () => 'Connect a mailbox.',
    indexHint: () => 'The index keeps only each message’s details.',
    actionsOff: () => 'Nothing more is changed.',
  },
  // The core's own team screens (invitations into a team, signed in) and their flows.
  teams: true,
  syncPause: true,
  serverRole: true,
}
