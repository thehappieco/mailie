// Asking for the step-up before a call that needs one (docs/key-scheme.md
// section 11): linking a mailbox, giving Read with a grant, supplying a
// mailbox's key, its first key and a personal mailbox's new one. The server
// refuses those when the session's step-up time is more than ten minutes
// old, with the same 403 (not_authorized) as a role it refuses, so the
// console asks first, from the session's step-up time by the server's clock
// (state/session.ts freshStepUp), rather than after a refusal.
//
// There is one prompt for the whole page (components/StepUpPrompt.vue,
// mounted by App.vue over whatever dialog is open): a flow that needs a
// step-up waits for it, and goes on once it is given, or stops without a
// word when the person closes it (StepUpCancelled). A sign-out closes it.

import { reactive, watch } from 'vue'
import { ApiError } from '../api/http'
import { serverNow } from './connection'
import { freshStepUp, identity, STEP_UP_MARGIN_S } from './session'

/** The person closed the step-up prompt: the flow that asked stops, and says nothing. */
export class StepUpCancelled extends Error {
  constructor() {
    super('The step-up was not given.')
    this.name = 'StepUpCancelled'
  }
}

/** Whether the prompt is open. */
export const stepUpPrompt = reactive<{ open: boolean }>({ open: false })

interface Waiter { resolve: () => void; reject: (error: Error) => void }
let waiting: Waiter[] = []

function settle(given: boolean): void {
  stepUpPrompt.open = false
  const waiters = waiting
  waiting = []
  for (const waiter of waiters) {
    if (given) waiter.resolve()
    else waiter.reject(new StepUpCancelled())
  }
}

/** The prompt's step-up went through: every flow waiting for it goes on. */
export function stepUpGiven(): void {
  settle(true)
}

/** The prompt was closed without a step-up: every flow waiting for it stops. */
export function stepUpRefused(): void {
  settle(false)
}

// Another person, or nobody: nothing waits for a step-up of the one before.
watch(identity, () => { if (stepUpPrompt.open || waiting.length) stepUpRefused() }, { flush: 'sync' })

/** askStepUp opens the prompt, or joins the one open, and settles when it is given or closed. */
export function askStepUp(): Promise<void> {
  return new Promise((resolve, reject) => {
    waiting.push({ resolve, reject })
    stepUpPrompt.open = true
  })
}

/**
 * ensureStepUp asks for the step-up unless the session's is fresh with
 * margin seconds to spare. It rejects with StepUpCancelled when the person
 * closes the prompt.
 */
export async function ensureStepUp(margin = STEP_UP_MARGIN_S): Promise<void> {
  if (!freshStepUp(serverNow(), margin)) await askStepUp()
}

/**
 * withStepUp makes a call that needs a fresh step-up: asked first when the
 * session's is not, with margin seconds to spare. A refusal (not_authorized)
 * after which the step-up is no longer fresh was the step-up going stale on
 * the way, not a role: it is asked again, and the call made once more; any
 * other refusal is the call's. call is run again whole, so whatever it seals
 * is sealed anew.
 */
export async function withStepUp<T>(call: () => Promise<T>, margin = STEP_UP_MARGIN_S): Promise<T> {
  await ensureStepUp(margin)
  try {
    return await call()
  } catch (error) {
    if (!(error instanceof ApiError && error.code === 'not_authorized') || freshStepUp()) throw error
    await askStepUp()
    return call()
  }
}
