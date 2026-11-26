// An invitation into a team, as the console renders it to someone signed in
// (server-side, as render.spec.ts does): what joining gives, and what an
// invitation for another address offers instead. Every rule shown here is
// one the server enforces; these hold the console to saying it beforehand.
import { afterEach, describe, expect, it } from 'vitest'
import { createSSRApp, h, type Component } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import type { Workspace } from '../src/api/types'
import InvitationDialog from '../src/components/InvitationDialog.vue'
import { invitation } from '../src/state/invitation'
import { session } from '../src/state/session'
import { workspaces } from '../src/state/workspaces'
import { locale } from '../src/ui/i18n'
import { ana } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }

async function render(component: Component, props: Record<string, unknown> = {}): Promise<string> {
  const context: SSRContext = {}
  const html = await renderToString(createSSRApp({ render: () => h(component, props) }), context)
  return html + Object.values(context.teleports ?? {}).join('')
}
/** The text of the markup, without its tags. */
const text = (html: string) => html.replace(/<!--[^]*?-->/g, '').replace(/<[^>]+>/g, ' ').replace(/&#39;/g, '\'').replace(/&amp;/g, '&').replace(/\s+/g, ' ').trim()

/** Signed in as Ana, showing workspace id, with these workspaces. */
function showing(id: string, list: Workspace[]) {
  Object.assign(session, { phase: 'ready', user: ana, expiresAt: Math.floor(Date.now() / 1000) + 86_400 })
  Object.assign(workspaces, { list, loaded: true, supported: true, currentID: id, failure: null, lost: null })
}

afterEach(() => {
  Object.assign(session, { phase: 'signed-out', user: null, expiresAt: 0 })
  Object.assign(workspaces, { list: [], loaded: false, supported: false, currentID: '', failure: null, lost: null })
  Object.assign(invitation, { pending: null, busy: false, problem: null, joined: null })
  locale.value = 'en'
})

describe('an invitation opened signed in', () => {
  it('asks to join, saying joining gives access to no mailbox', async () => {
    showing(PERSONAL, [personal])
    Object.assign(invitation, { pending: { invite: 'code', email: 'Ana@Example.test' } })
    const html = await render(InvitationDialog)
    expect(text(html)).toContain('This invitation is for Ana@Example.test.')
    expect(text(html)).toContain('Joining gives you access to no mailbox')
    expect(text(html)).toContain('Join the team')
    expect(html).not.toContain('>code<')
  })

  it('says an invitation for another address works only for it, and offers signing out instead of joining', async () => {
    showing(PERSONAL, [personal])
    Object.assign(invitation, { pending: { invite: 'code', email: 'bea@example.test' } })
    const html = await render(InvitationDialog)
    expect(text(html)).toContain('This invitation is for bea@example.test, and you are signed in as ana@example.test. It works only for its own address: sign out to use it.')
    expect(text(html)).not.toContain('Join the team')
    expect(text(html)).toContain('Sign out')
  })
})
