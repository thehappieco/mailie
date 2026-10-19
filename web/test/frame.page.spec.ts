// The pieces a host draws its own frame with, on a page (test/dom.ts): what
// keeps the console going while a frame is on screen (useConsoleFrame), the
// page's life around its phases (useAppLifecycle, AppOverlays), the core's own
// frame, which alone owns the page's scrolling (useFixedFrame), the row of
// the person's sessions (SessionsRow), and the core's frame for an edition
// whose person's settings are elsewhere (ConsoleShell without an account
// section, with links). The open console builds on the same pieces, and its
// own specs hold what it shows.
import './dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { click, find, flush, page, words } from './dom'
import { mount, type Mounted } from './mount'
import type { Workspace } from '../src/api/types'
import App from '../src/App.vue'
import AppOverlays from '../src/components/AppOverlays.vue'
import ConsoleShell, { type ConsoleLinks, type ConsoleSection } from '../src/components/ConsoleShell.vue'
import RestoringView from '../src/components/RestoringView.vue'
import SessionsRow from '../src/components/SessionsRow.vue'
import SignInView from '../src/components/SignInView.vue'
import { configureEdition } from '../src/edition'
import { openEdition } from '../src/open/edition'
import { accounts } from '../src/state/accounts'
import { useAppLifecycle } from '../src/state/appLifecycle'
import { useConsoleFrame } from '../src/state/consoleFrame'
import { server } from '../src/state/connection'
import { adoptSession, session, signOut } from '../src/state/session'
import { workspaces } from '../src/state/workspaces'
import { announcement } from '../src/ui/announce'
import { FIXED_FRAME_CLASS } from '../src/ui/frame'
import { failure, json, reply, stubPage } from './support'

const PERSONAL = 'wsp_000000000000aaaa'
const TEAM = 'wsp_000000000000bbbb'
const personal: Workspace = { id: PERSONAL, kind: 'personal', source: 'local', name: '', role: 'owner', status: 'active', created_at: 1_790_000_000 }
const support: Workspace = { id: TEAM, kind: 'team', source: 'local', name: 'Support', role: 'admin', status: 'active', created_at: 1_790_000_000 }

interface Seen { path: string; method: string; query: Record<string, string>; body: unknown; signal: AbortSignal | null }

let mounted: Mounted[] = []
let seen: Seen[] = []
let listed: Workspace[] = []
/** What POST /v1/auth/logout answers. */
let logout: () => Response = () => new Response(null, { status: 204 })

/** A daemon with the routes a frame reads; the event stream stays open until the page stops it. */
function daemon() {
  vi.spyOn(globalThis, 'fetch').mockImplementation(async (input, init) => {
    const url = new URL(String(input))
    const request: Seen = {
      path: url.pathname, method: init?.method ?? 'GET', query: Object.fromEntries(url.searchParams),
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined, signal: init?.signal ?? null,
    }
    seen.push(request)
    switch (request.path) {
      case '/v1/auth/me': return failure('unauthorized', 401)
      case '/v1/auth/logout': return logout()
      case '/v1/workspaces': return json(listed)
      case '/v1/accounts': return json([])
      case '/v1/providers': return json([])
      case '/v1/me/notices': return json([])
      case '/v1/me/sync-consent':
      case '/v1/me/actions-consent': return json({ consented: false, current_version: 'v1' })
      case '/v1/events': return new Promise<Response>((_, reject) => {
        request.signal?.addEventListener('abort', () => reject(new DOMException('The operation was aborted.', 'AbortError')))
      })
    }
    return failure('not_found', 404)
  })
}
const asked = (path: string) => seen.filter(request => request.path === path)

beforeEach(() => {
  stubPage()
  seen = []
  listed = [personal]
  logout = () => new Response(null, { status: 204 })
  daemon()
})
afterEach(async () => {
  for (const one of mounted) one.unmount()
  mounted = []
  await signOut().catch(() => {})
  configureEdition(openEdition)
  page.documentElement.classList.remove(FIXED_FRAME_CLASS)
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

/** A host's own frame: the core's panels would go where the notice is. */
const HostFrame = defineComponent({
  setup() {
    const { workspaceNotice, dismissWorkspaceNotice } = useConsoleFrame()
    return () => h('main', { class: 'console-surface console-content' }, workspaceNotice.value
      ? [h('p', { class: 'workspace-notice' }, workspaceNotice.value), h('button', { type: 'button', onClick: dismissWorkspaceNotice }, 'Dismiss')]
      : [])
  },
})

/** A host's root: the phases, drawn in its own frame, the sign-in card framed, and what goes over them. */
const HostRoot = defineComponent({
  setup() {
    const { finishing } = useAppLifecycle()
    return () => h('div', { class: 'host-frame' }, [
      session.phase === 'restoring' ? h(RestoringView) : session.phase === 'signed-out' ? h(SignInView, { invitation: null, framed: true }) : finishing.value ? null : h('p', 'Signed in'),
      h(AppOverlays, { finishing: finishing.value }),
    ])
  },
})

describe('a frame of a host’s own', () => {
  it('keeps the console going while it is on screen: the workspaces, the mailboxes of the one shown, the providers, the agreements, and the stream', async () => {
    await adoptSession(reply())
    mounted.push(mount(HostFrame))
    await vi.waitFor(() => expect(accounts.loaded).toBe(true))
    await vi.waitFor(() => expect(asked('/v1/events')).toHaveLength(1))
    expect(asked('/v1/workspaces')).toHaveLength(1)
    expect(asked('/v1/accounts').map(request => request.query)).toEqual([{ workspace: PERSONAL }])
    expect(asked('/v1/providers')).toHaveLength(1)
    expect(asked('/v1/me/sync-consent')).toHaveLength(1)
    expect(asked('/v1/me/actions-consent')).toHaveLength(1)
    // The stream narrows to the workspace shown.
    expect(asked('/v1/events')[0]!.query).toEqual({ workspace: PERSONAL })
    expect(server.stream).not.toBe('off')

    // Gone from the screen: the stream stops, and nothing is read again.
    const before = seen.length
    mounted.pop()!.unmount()
    await flush()
    expect(asked('/v1/events')[0]!.signal?.aborted).toBe(true)
    expect(server.stream).toBe('off')
    expect(seen.length).toBe(before)
  })

  it('says when the workspace shown goes away, and which one it shows now, until dismissed', async () => {
    listed = [personal, support]
    await adoptSession(reply())
    mounted.push(mount(HostFrame))
    await vi.waitFor(() => expect(workspaces.loaded).toBe(true))
    workspaces.currentID = TEAM
    listed = [personal]
    const { loadWorkspaces } = await import('../src/state/workspaces')
    await loadWorkspaces()
    await flush()
    const said = 'You are no longer a member of Support. This page now shows Personal.'
    expect(words(find('.workspace-notice')!)).toBe(said)
    expect(announcement.value).toBe(said)
    await click(find('button', 'Dismiss'))
    expect(find('.workspace-notice')).toBeNull()
  })
})

describe('the page’s life around its phases', () => {
  it('the core’s own frame owns the page’s scrolling while it is mounted, and only it', async () => {
    const app = mount(App)
    mounted.push(app)
    expect(page.documentElement.classList.contains(FIXED_FRAME_CLASS)).toBe(true)
    // No session remembered: signed out, the card with the brand and the appearance menu above it.
    await vi.waitFor(() => expect(session.phase).toBe('signed-out'))
    await flush()
    expect(find('.unlock .auth-top')).not.toBeNull()
    expect(page.documentElement.style['--app-height']).toBe('900px')
    mounted.pop()
    app.unmount()
    expect(page.documentElement.classList.contains(FIXED_FRAME_CLASS)).toBe(false)
    expect(page.documentElement.style['--app-height']).toBeUndefined()
  })

  it('runs the same for a host’s root, whose page scrolls: restoring, then the sign-in card in its frame, and what goes over it', async () => {
    mounted.push(mount(HostRoot))
    expect(find('.host-frame [role=status]')).not.toBeNull()
    await vi.waitFor(() => expect(session.phase).toBe('signed-out'))
    await flush()
    expect(page.documentElement.classList.contains(FIXED_FRAME_CLASS)).toBe(false)
    expect(find('.host-frame .unlock.framed')).not.toBeNull()
    expect(find('.auth-top')).toBeNull()
    expect(words(find('.host-frame h1')!)).toBe('Sign in')
    // The live region is there before anything is said through it.
    expect(find('.host-frame [aria-live]')).not.toBeNull()

    // Signed in, and this browser would not keep the session: the line over the page says so.
    await adoptSession(reply())
    session.notRemembered = true
    await flush()
    expect(words(find('.session-notice')!)).toContain('This browser did not let Mailie remember your session.')
    await click(find('.session-notice button'))
    expect(find('.session-notice')).toBeNull()
  })
})

describe('the row of the person’s sessions', () => {
  it('signs out here, and everywhere only once asked again', async () => {
    await adoptSession(reply())
    mounted.push(mount(SessionsRow))
    expect(words(find('.sessions-row')!)).toContain('Sessions')
    expect(words(find('.sessions-row')!)).toContain('This session lasts until')
    await click(find('button', 'Sign out everywhere'))
    // Asked again, nothing sent yet; and taken back.
    expect(asked('/v1/auth/logout')).toHaveLength(0)
    await click(find('button', 'Cancel'))
    await click(find('button', 'Sign out everywhere'))
    await click(find('button', 'Sign out everywhere now'))
    await vi.waitFor(() => expect(session.phase).toBe('signed-out'))
    expect(asked('/v1/auth/logout').map(request => request.body)).toEqual([{ everywhere: true }])

    await adoptSession(reply())
    await flush()
    await click(find('.sessions-row button', 'Sign out'))
    await vi.waitFor(() => expect(session.phase).toBe('signed-out'))
    expect(asked('/v1/auth/logout').map(request => request.body)).toEqual([{ everywhere: true }, { everywhere: false }])
  })

  it('says why signing out everywhere did not happen, still signed in', async () => {
    logout = () => failure('unavailable', 503)
    await adoptSession(reply())
    mounted.push(mount(SessionsRow))
    await click(find('button', 'Sign out everywhere'))
    await click(find('button', 'Sign out everywhere now'))
    await vi.waitFor(() => expect(find('.sessions-row [role=alert]')).not.toBeNull())
    expect(session.phase).toBe('ready')
    expect(find('button', 'Sign out everywhere now')).toBeNull()
  })

  it('takes a host’s own words for its title, its line and its button', async () => {
    await adoptSession(reply())
    mounted.push(mount(SessionsRow, { title: 'Signed in here', description: 'Until tomorrow.', signOutLabel: 'Leave this page’s session' }))
    const row = words(find('.sessions-row')!)
    expect(row).toContain('Signed in here')
    expect(row).toContain('Until tomorrow.')
    expect(row).not.toContain('This session lasts until')
    expect(find('button', 'Leave this page’s session')).not.toBeNull()
  })
})

/** The one section of an edition whose person's own settings are elsewhere. */
const Reading = defineComponent({ setup: () => () => h('p', { class: 'reading' }, 'Your mail') })
const reading: ConsoleSection[] = [{ id: 'mail', label: 'Mail', description: 'Read your mail.', icon: 'inbox', component: Reading }]
const elsewhere: ConsoleLinks = {
  label: 'Settings',
  items: [
    { id: 'mailboxes', label: 'Email accounts', icon: 'mail', href: '/screens/mailboxes?workspace=w1' },
    { id: 'permissions', label: 'Permissions', icon: 'shield', href: '/screens/permissions?workspace=w1' },
  ],
}
/** Each link of a group of links, as its label and where it goes. */
const linked = (selector: string) => page.querySelectorAll(`${selector} a`).map(link => `${words(link)} → ${link.getAttribute('href')}`)

describe('the core’s frame, for an edition whose person’s settings are elsewhere', () => {
  it('links to them after its sections, as plain links under the group’s name, in the sidebar and in the drawer', async () => {
    await adoptSession(reply())
    mounted.push(mount(ConsoleShell, { sections: reading, home: 'mail', links: elsewhere }))
    await flush()
    const want = ['Email accounts → /screens/mailboxes?workspace=w1', 'Permissions → /screens/permissions?workspace=w1']
    const group = find('.console-sidebar nav.console-links')!
    expect(group.getAttribute('aria-label')).toBe('Settings')
    expect(words(group.querySelector('.console-links-label')!)).toBe('Settings')
    expect(linked('.console-sidebar nav.console-links')).toEqual(want)
    // Links, never the frame's own sections: no button, nothing current.
    expect(group.querySelectorAll('button')).toEqual([])
    expect(group.querySelectorAll('[aria-current]')).toEqual([])
    expect(page.querySelectorAll('.console-sidebar nav').map(nav => nav.getAttribute('aria-label'))).toEqual(['Sections', 'Settings'])
    expect(words(find('.reading')!)).toBe('Your mail')

    await click(find('.mobile-menu'))
    expect(linked('.mobile-navigation nav.console-links')).toEqual(want)
    expect(find('.mobile-navigation nav.console-links')!.getAttribute('aria-label')).toBe('Settings')
  })

  it('says who is signed in at the sidebar’s foot, as text that opens nothing, and keeps signing out', async () => {
    await adoptSession(reply())
    mounted.push(mount(ConsoleShell, { sections: reading, home: 'mail', links: elsewhere }))
    await flush()
    expect(find('.profile-trigger')).toBeNull()
    expect(find('.mobile-profile')).toBeNull()
    const who = (selector: string) => ['strong', 'small'].map(part => words(find(`${selector} .profile-who ${part}`)!))
    expect(who('.console-sidebar')).toEqual(['Ana Souza', 'ana@example.test'])
    await click(find('.mobile-menu'))
    expect(who('.mobile-account-area')).toEqual(['Ana Souza', 'ana@example.test'])
    expect(find('.mobile-account-area .profile-trigger')).toBeNull()

    await click(find('.console-sidebar .profile-signout'))
    await vi.waitFor(() => expect(session.phase).toBe('signed-out'))
    expect(asked('/v1/auth/logout').map(request => request.body)).toEqual([{ everywhere: false }])
  })

  it('with the person’s own section and no links, is the frame it always was', async () => {
    await adoptSession(reply())
    const own = defineComponent({ setup: () => () => h('p', { class: 'own' }, 'Yours') })
    mounted.push(mount(ConsoleShell, {
      sections: [...reading, { id: 'account', label: 'Account', description: 'Yours.', icon: 'user', component: own, scope: 'person' }],
      home: 'mail', account: 'account',
    }))
    await flush()
    expect(find('.console-links')).toBeNull()
    expect(find('.profile-who')).toBeNull()
    expect(find('.mobile-profile')!.getAttribute('aria-label')).toBe('Account')
    await click(find('.console-sidebar .profile-trigger'))
    expect(words(find('.own')!)).toBe('Yours')
    expect(find('.console-sidebar [aria-current]')!.textContent).toContain('Account')
  })
})
