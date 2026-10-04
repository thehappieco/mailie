// The person's consent to actions on their messages, as the core keeps it:
// the open console's Account section switches it, and a key that can act is
// offered only while it stands.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ACTIONS_TEXT_VERSION } from '../src/open/versions'
import { freshModules, json, reply, serve, stubPage, type Route } from './support'

const notYet = { consented: false, current_version: ACTIONS_TEXT_VERSION }
const given = { consented: true, consented_at: 1_790_000_000, version: ACTIONS_TEXT_VERSION, current_version: ACTIONS_TEXT_VERSION }

async function signedIn(route: Route) {
  await freshModules()
  const session = await import('../src/state/session')
  const consent = await import('../src/state/actionsConsent')
  const fetch = serve(request => {
    if (request.path === '/v1/auth/login') return json(reply())
    if (request.path === '/v1/auth/logout') return new Response(null, { status: 204 })
    return route(request)
  })
  await session.signIn('ana@example.test', 'correct-password')
  return { session, consent, fetch }
}

const bodies = (fetch: ReturnType<typeof serve>, method: string) =>
  fetch.mock.calls.filter(([url, init]) => new URL(String(url)).pathname === '/v1/me/actions-consent' && (init?.method ?? 'GET') === method)
    .map(([, init]) => typeof init?.body === 'string' ? JSON.parse(init.body) : undefined)

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('consent to actions', () => {
  it('allows actions with the revision of the text the edition shows, and takes it back with DELETE, telling whoever listens', async () => {
    let state: object = notYet
    const { consent, fetch } = await signedIn(({ method }) => {
      if (method === 'POST') state = given
      if (method === 'DELETE') state = notYet
      return json(state)
    })
    const heard: string[] = []
    consent.onActionsConsent(change => heard.push(`${change}:${consent.actionsAllowed()}`))
    await consent.loadActionsConsent()
    expect(consent.actionsStanding()).toBe('off')
    expect(await consent.allowActions()).toBe(true)
    expect(bodies(fetch, 'POST')).toEqual([{ version: ACTIONS_TEXT_VERSION }])
    expect(consent.actionsStanding()).toBe('allowed')
    expect(await consent.stopActions()).toBe(true)
    expect(bodies(fetch, 'DELETE')).toHaveLength(1)
    // Said no just now: nothing asks again at once.
    expect(consent.actionsConsent.dismissed).toBe(true)
    expect(heard).toEqual(['adopted:false', 'adopted:true', 'granted:true', 'adopted:false', 'withdrawn:false'])
  })

  it('agrees to nothing once the server names another revision than its text, and pauses a consent to an older one', async () => {
    const { consent, fetch } = await signedIn(() => json({ consented: true, consented_at: 1_790_000_000, version: ACTIONS_TEXT_VERSION, current_version: '2027-01-open-actions' }))
    await consent.loadActionsConsent()
    expect(consent.actionsTextOutdated()).toBe(true)
    expect(consent.actionsOutdated()).toBe(true)
    expect(consent.actionsAllowed()).toBe(false)
    expect(consent.actionsStanding()).toBe('outdated')
    expect(await consent.allowActions()).toBe(false)
    expect(bodies(fetch, 'POST')).toEqual([])
  })

  it('forgets the answer when the person changes', async () => {
    const { session, consent } = await signedIn(() => json(given))
    await consent.loadActionsConsent()
    expect(consent.actionsAllowed()).toBe(true)
    await session.signOut()
    expect(consent.actionsConsent.loaded).toBe(false)
    expect(consent.actionsAllowed()).toBe(false)
  })
})
