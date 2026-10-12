// The fake page (test/dom.ts) has what controls an edition may draw in the
// frame touch as they mount and move: data-* attributes through dataset,
// classes through classList, and animation frames, which run once the page
// settles. The core's theme writes on the same dataset.
import './dom'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h, onBeforeUnmount, onMounted, ref } from 'vue'
import { flush, page } from './dom'
import { mount, type Mounted } from './mount'
import { setTheme } from '../src/ui/preferences'
import { stubPage } from './support'

let mounted: Mounted | null = null
afterEach(() => {
  mounted?.unmount()
  mounted = null
  vi.unstubAllGlobals()
})

describe('the fake page', () => {
  it('reads and writes data-* attributes through dataset, by their camelCase names', () => {
    const element = page.createElement('span')
    element.dataset.thcHover = ''
    expect(element.getAttribute('data-thc-hover')).toBe('')
    element.setAttribute('data-place', 'menu')
    expect(element.dataset.place).toBe('menu')
    expect('thcHover' in element.dataset).toBe(true)
    expect(Object.keys(element.dataset)).toEqual(['thcHover', 'place'])
    expect({ ...element.dataset }).toEqual({ thcHover: '', place: 'menu' })
    delete element.dataset.thcHover
    expect(element.hasAttribute('data-thc-hover')).toBe(false)
    expect(element.dataset.thcHover).toBeUndefined()
    expect('thcHover' in element.dataset).toBe(false)
  })

  it('keeps classList and the class attribute one and the same', () => {
    const element = page.createElement('button')
    element.className = 'thc-toggle'
    element.classList.add('open', 'thc-toggle')
    expect(element.getAttribute('class')).toBe('thc-toggle open')
    expect(element.classList.contains('open')).toBe(true)
    expect(element.matches('.open')).toBe(true)
    expect(element.classList.toggle('open')).toBe(false)
    expect(element.classList.contains('open')).toBe(false)
    expect(element.classList.toggle('open', true)).toBe(true)
    expect(element.classList.toggle('open', true)).toBe(true)
    element.classList.remove('thc-toggle', 'absent')
    expect(element.className).toBe('open')
  })

  it('runs an animation frame once the page settles, with its time, and never one cancelled', async () => {
    const ran: number[] = []
    const kept = requestAnimationFrame(time => ran.push(time))
    const dropped = window.requestAnimationFrame(() => ran.push(-1))
    expect(kept).not.toBe(dropped)
    window.cancelAnimationFrame(dropped)
    expect(ran).toEqual([])
    await flush()
    expect(ran).toHaveLength(1)
    expect(ran[0]).toBeGreaterThan(0)
  })

  it('gives a mounted component all three, as a control that animates on hover uses them', async () => {
    const Control = defineComponent({
      setup() {
        const root = ref<HTMLElement>()
        let frame = 0
        onMounted(() => {
          root.value!.dataset.thcHover = ''
          frame = requestAnimationFrame(() => root.value!.classList.add('played'))
        })
        onBeforeUnmount(() => cancelAnimationFrame(frame))
        return () => h('span', { ref: root, class: 'control' })
      },
    })
    mounted = mount(Control)
    const control = page.querySelector('.control')!
    expect(control.hasAttribute('data-thc-hover')).toBe(true)
    expect(control.classList.contains('played')).toBe(false)
    await flush()
    expect(control.getAttribute('class')).toBe('control played')
  })

  it('takes the core’s theme on the document’s dataset', () => {
    stubPage()
    setTheme('dark')
    expect(page.documentElement.getAttribute('data-theme')).toBe('dark')
    setTheme('light')
    expect(page.documentElement.dataset.theme).toBe('light')
  })
})
