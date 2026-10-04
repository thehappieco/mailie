// Mounting a component on test/dom.ts's page. Imported after it: Vue must
// find that page's document when it loads.
import { createApp, h, type App, type Component } from 'vue'
import { page, type FakeElement } from './dom'

export interface Mounted { app: App; root: FakeElement; unmount(): void }

/** Mounts a component with these props into the page's body. */
export function mount(component: Component, props: Record<string, unknown> = {}): Mounted {
  const root = page.createElement('div')
  page.body.appendChild(root)
  const app = createApp({ render: () => h(component, props) })
  app.mount(root as unknown as Element)
  return { app, root, unmount() { app.unmount(); root.remove() } }
}
