// The core's seams for an edition whose people prove themselves again another
// way than a password (Edition.stepUp and Edition.copy.stepUpHint), and whose
// sign-in opens or makes the account key itself (adoptSession, which begins
// keyed for the person it names). The open edition sets none of them, and
// keeps its password dialog and sentences.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createSSRApp, h } from 'vue'
import { renderToString, type SSRContext } from 'vue/server-renderer'
import { generateAccountKeys } from '@thehappieco/kit/account'
import { toBase64URL } from '@thehappieco/kit/bytes'
import type { Edition, StepUpReason } from '../src/edition'
import { ana, now, reply, serve, settle, stubPage } from './support'

const BEA_SEAL = '0f4c2a1e-7d3b-4e8a-9c61-52b7e0d9a3f4'

/** A promise the spec settles. */
function deferred() {
  let resolve!: () => void
  let reject!: (error: Error) => void
  const promise = new Promise<void>((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

/** A fresh page whose edition is the open one with extra, and Ana signed in with no step-up time. */
async function load(extra: Partial<Edition> = {}) {
  vi.resetModules()
  const [{ configureEdition }, { openEdition }] = await Promise.all([import('../src/edition'), import('../src/open/edition')])
  configureEdition({ ...openEdition, ...extra })
  const [session, stepUp, vault, prompt] = await Promise.all([
    import('../src/state/session'), import('../src/state/stepUp'), import('../src/state/accountVault'),
    import('../src/components/StepUpPrompt.vue'),
  ])
  await session.adoptSession({ ...reply(), authenticated_at: 0 })
  return { ...session, ...stepUp, vault, StepUpPrompt: prompt.default }
}

/** What the page's step-up prompt draws now, its dialog's teleported markup included. */
async function drawn(component: Parameters<typeof h>[0]): Promise<string> {
  const context: SSRContext = {}
  const html = await renderToString(createSSRApp({ render: () => h(component) }), context)
  return html + Object.values(context.teleports ?? {}).join('')
}

beforeEach(() => { stubPage() })
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('an edition’s own step-up', () => {
  it('is asked once for every flow waiting, with the first one’s reason, in place of the password dialog', async () => {
    const asked = deferred()
    const stepUp = vi.fn((_reason: StepUpReason) => asked.promise)
    const s = await load({ stepUp })
    const calls: string[] = []
    const link = s.withStepUp('link', async () => { calls.push('link'); return 'linked' })
    const key = s.ensureStepUp('key')
    await settle()
    expect(stepUp).toHaveBeenCalledTimes(1)
    expect(stepUp).toHaveBeenCalledWith('link')
    expect(s.stepUpPrompt.open).toBe(true)
    expect(calls).toEqual([])
    // Nothing of the password dialog.
    const html = await drawn(s.StepUpPrompt)
    expect(html).not.toContain('Enter your password again')
    expect(html).not.toContain('name="password"')

    // The edition records the step-up its route answered, then resolves.
    s.steppedUp(ana.id, now())
    asked.resolve()
    await expect(link).resolves.toBe('linked')
    await expect(key).resolves.toBeUndefined()
    expect(calls).toEqual(['link'])
    expect(s.stepUpPrompt.open).toBe(false)
    // Fresh now: a flow that follows asks nothing.
    await s.ensureStepUp('grant')
    expect(stepUp).toHaveBeenCalledTimes(1)
  })

  it('stops every flow waiting without a word when the person gives up, and with the edition’s error otherwise', async () => {
    let answer: () => Promise<void> = () => Promise.reject(new Error('unset'))
    const s = await load({ stepUp: () => answer() })
    answer = () => Promise.reject(new s.StepUpCancelled())
    const first = s.ensureStepUp('grant')
    const second = s.ensureStepUp('key')
    await expect(first).rejects.toBeInstanceOf(s.StepUpCancelled)
    await expect(second).rejects.toBeInstanceOf(s.StepUpCancelled)
    expect(s.stepUpPrompt.open).toBe(false)

    const blocked = new Error('the window was blocked')
    answer = () => Promise.reject(blocked)
    const call = vi.fn(async () => 'sent')
    await expect(s.withStepUp('key', call)).rejects.toBe(blocked)
    expect(call).not.toHaveBeenCalled()
  })

  it('settles only the prompt it was asked for: a sign-out stops the flows, and its late answer opens nothing', async () => {
    const answers = [deferred(), deferred()]
    let n = 0
    const stepUp = vi.fn(() => answers[n++]!.promise)
    const s = await load({ stepUp })
    serve(() => new Response(null, { status: 204 }))
    const before = s.ensureStepUp('link').catch((error: unknown) => error)
    await settle()
    await s.signOut()
    expect(await before).toBeInstanceOf(s.StepUpCancelled)

    // Signed in again, a flow asks anew; the first answer, late, settles
    // nothing of it.
    await s.adoptSession({ ...reply('tok_second_000000000000000000000000000000000'), authenticated_at: 0 })
    let done = false
    const after = s.ensureStepUp('key').then(() => { done = true })
    await settle()
    expect(stepUp).toHaveBeenCalledTimes(2)
    answers[0]!.resolve()
    await settle()
    expect(done).toBe(false)
    expect(s.stepUpPrompt.open).toBe(true)
    answers[1]!.resolve()
    await after
    expect(done).toBe(true)
  })

  it('words the sentence beside a guarded write as the edition says, and the open edition’s names the password', async () => {
    let s = await load()
    expect(s.stepUpHint()).toBe('It asks for your password if you have not entered it in the last ten minutes.')
    s.stepUpPrompt.open = true
    expect(await drawn(s.StepUpPrompt)).toContain('Enter your password again')

    const { openEdition } = await import('../src/open/edition')
    s = await load({
      stepUp: async () => {},
      copy: { ...openEdition.copy, stepUpHint: () => 'It asks you to sign in again if you have not in the last ten minutes.' },
    })
    expect(s.stepUpHint()).toBe('It asks you to sign in again if you have not in the last ten minutes.')
  })
})

describe('the session an edition’s sign-in adopts, and the account key', () => {
  async function page() {
    vi.resetModules()
    const [{ configureEdition }, { openEdition }] = await Promise.all([import('../src/edition'), import('../src/open/edition')])
    configureEdition(openEdition)
    const [session, vault] = await Promise.all([import('../src/state/session'), import('../src/state/accountVault')])
    return { ...session, vault }
  }

  it('begins keyed when the sign-in kept the key of the person it names', async () => {
    const s = await page()
    const pair = await generateAccountKeys()
    const person = { ...ana, public_key: toBase64URL(pair.publicKey) }
    await s.vault.keepAccountKey(pair.privateKey, pair.publicKey, person.seal_id!)
    await s.adoptSession(reply(undefined, person))
    expect(s.session.phase).toBe('ready')
    expect(s.session.keyed).toBe(true)
    expect(await s.vault.holdsAccountKey(person.seal_id!, person.public_key)).toBe(true)
  })

  it('wipes a key of anyone else, and begins without one', async () => {
    const s = await page()
    const bea = await generateAccountKeys()
    await s.vault.keepAccountKey(bea.privateKey, bea.publicKey, BEA_SEAL)
    await s.adoptSession(reply())
    expect(s.session.keyed).toBe(false)
    expect(await s.vault.holdsAccountKey(BEA_SEAL, toBase64URL(bea.publicKey))).toBe(false)
  })

  it('begins without a key for a person who has none, and for one whose key this browser does not hold', async () => {
    const s = await page()
    await s.adoptSession(reply(undefined, { ...ana, public_key: undefined }))
    expect(s.session.keyed).toBe(false)
    await s.adoptSession(reply('tok_second_000000000000000000000000000000000'))
    expect(s.session.keyed).toBe(false)
  })
})
