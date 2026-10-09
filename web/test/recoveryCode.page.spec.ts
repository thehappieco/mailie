// A recovery code on a page (test/dom.ts): shown once, over whatever is on
// screen, and gone only once the person says they saved it.
import './dom'
import { afterEach, describe, expect, it } from 'vitest'
import { click, check, fire, find, flush, page, words } from './dom'
import { mount, type Mounted } from './mount'
import RecoveryCodeDialog from '../src/components/RecoveryCodeDialog.vue'
import { recoveryCode } from '../src/state/account'

const CODE = '01234-56789-ABCDE-FGHJK-MNPQR-STVWX'
let mounted: Mounted | null = null

afterEach(() => {
  mounted?.unmount()
  mounted = null
  Object.assign(recoveryCode, { code: '', reason: '' })
})

describe('a new recovery code on a page', () => {
  it('stays until the person says they saved it, and is forgotten then', async () => {
    Object.assign(recoveryCode, { code: CODE, reason: 'new-account' })
    mounted = mount(RecoveryCodeDialog)
    await flush()
    expect(words(find('dialog .code-secret')!)).toBe(CODE)
    expect(words(find('dialog')!)).toContain('this code is the only way back into your account')
    const proceed = find('dialog button.primary', 'Continue')!
    expect(proceed.disabled).toBe(true)
    // Escape and the close button leave it open, and say why.
    await fire(find('dialog'), 'cancel')
    await click(find('dialog button[aria-label="Close"]'))
    expect(recoveryCode.code).toBe(CODE)
    expect(words(find('dialog .alert')!)).toBe('Confirm that you saved the code first: it cannot be shown again.')
    await check(find('dialog input[name=recovery-code-saved]'))
    expect(find('dialog button.primary', 'Continue')!.disabled).toBe(false)
    await click(find('dialog button.primary', 'Continue'))
    expect(recoveryCode.code).toBe('')
    expect(page.querySelectorAll('dialog .code-secret')).toHaveLength(0)
  })

  it('says the old code no longer works when it replaces one', async () => {
    Object.assign(recoveryCode, { code: CODE, reason: 'replaced' })
    mounted = mount(RecoveryCodeDialog)
    await flush()
    expect(words(find('dialog')!)).toContain('Your old recovery code no longer works.')
  })
})
