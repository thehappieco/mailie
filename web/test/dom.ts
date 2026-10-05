// Just enough of a DOM for Vue to mount a component and for a spec to press
// its buttons: nodes, attributes, listeners, focus, and <dialog>'s
// showModal. No layout, no CSS, no HTML parsing. The specs otherwise run in
// Node and render on the server; this is for what only an event can show
// (a dialog that stays open on a failure, a switch that asks first).
//
// Vue's DOM runtime reads `document` once, when it loads: a spec imports this
// file before anything that imports 'vue', and this file imports nothing
// (mount.ts has what needs Vue). Vitest runs each spec file on its own, so no
// other spec sees these globals.

type Listener = (event: FakeEvent) => void

export interface FakeEvent {
  type: string
  target: FakeNode
  currentTarget: FakeNode | null
  relatedTarget: FakeNode | null
  defaultPrevented: boolean
  key?: string
  ctrlKey?: boolean
  metaKey?: boolean
  shiftKey?: boolean
  altKey?: boolean
  isComposing?: boolean
  preventDefault(): void
  stopPropagation(): void
  stopImmediatePropagation(): void
}

class FakeNode {
  /** Never made reactive (Vue's markRaw): a template ref must be the node itself, as with a real DOM node. */
  readonly __v_skip = true
  parentNode: FakeNode | null = null
  childNodes: FakeNode[] = []
  listeners = new Map<string, Set<Listener>>()
  constructor(readonly nodeType: number, readonly nodeName: string) {}

  get parentElement(): FakeElement | null { return this.parentNode instanceof FakeElement ? this.parentNode : null }
  get firstChild(): FakeNode | null { return this.childNodes[0] ?? null }
  get lastChild(): FakeNode | null { return this.childNodes.at(-1) ?? null }
  get nextSibling(): FakeNode | null {
    const siblings = this.parentNode?.childNodes
    return siblings ? siblings[siblings.indexOf(this) + 1] ?? null : null
  }
  get previousSibling(): FakeNode | null {
    const siblings = this.parentNode?.childNodes
    return siblings ? siblings[siblings.indexOf(this) - 1] ?? null : null
  }
  get ownerDocument(): FakeDocument { return fakeDocument }
  get isConnected(): boolean { return this.getRootNode() === fakeDocument }
  getRootNode(): FakeNode {
    let node: FakeNode = this
    while (node.parentNode) node = node.parentNode
    return node
  }

  insertBefore(child: FakeNode, anchor: FakeNode | null): FakeNode {
    child.parentNode?.removeChild(child)
    const at = anchor ? this.childNodes.indexOf(anchor) : -1
    if (at < 0) this.childNodes.push(child)
    else this.childNodes.splice(at, 0, child)
    child.parentNode = this
    return child
  }
  appendChild(child: FakeNode): FakeNode { return this.insertBefore(child, null) }
  removeChild(child: FakeNode): FakeNode {
    const at = this.childNodes.indexOf(child)
    if (at >= 0) this.childNodes.splice(at, 1)
    child.parentNode = null
    return child
  }
  remove(): void { this.parentNode?.removeChild(this) }
  contains(node: unknown): boolean {
    for (let at = node instanceof FakeNode ? node : null; at; at = at.parentNode) if (at === this) return true
    return false
  }

  get textContent(): string { return this.childNodes.map(node => node.textContent).join('') }
  set textContent(value: string) {
    for (const node of this.childNodes) node.parentNode = null
    this.childNodes = []
    if (value) this.appendChild(new FakeText(value))
  }

  addEventListener(type: string, listener: Listener): void {
    let set = this.listeners.get(type)
    if (!set) this.listeners.set(type, set = new Set())
    set.add(listener)
  }
  removeEventListener(type: string, listener: Listener): void { this.listeners.get(type)?.delete(listener) }
  /** Calls the listeners here and on every ancestor, then the document's, as a bubbling event would. */
  dispatchEvent(event: FakeEvent): boolean {
    let stopped = false
    event.stopPropagation = () => { stopped = true }
    for (let node: FakeNode | null = this; node && !stopped; node = node.parentNode) {
      event.currentTarget = node
      for (const listener of [...node.listeners.get(event.type) ?? []]) listener(event)
    }
    return !event.defaultPrevented
  }
}

class FakeText extends FakeNode {
  constructor(public nodeValue: string) { super(3, '#text') }
  override get textContent(): string { return this.nodeValue }
  override set textContent(value: string) { this.nodeValue = value }
  get data(): string { return this.nodeValue }
}

class FakeComment extends FakeNode {
  constructor(public nodeValue: string) { super(8, '#comment') }
  override get textContent(): string { return '' }
}

class FakeElement extends FakeNode {
  readonly tagName: string
  readonly attributes = new Map<string, string>()
  readonly style: Record<string, unknown> = { setProperty(this: Record<string, string>, name: string, value: string) { this[name] = value }, removeProperty(this: Record<string, string>, name: string) { delete this[name] } }
  private field = ''
  _value?: unknown
  constructor(tag: string, readonly namespaceURI = 'http://www.w3.org/1999/xhtml') {
    super(1, tag.toUpperCase())
    this.tagName = tag.toUpperCase()
  }

  setAttribute(name: string, value: unknown): void { this.attributes.set(name, String(value)) }
  getAttribute(name: string): string | null { return this.attributes.get(name) ?? null }
  hasAttribute(name: string): boolean { return this.attributes.has(name) }
  removeAttribute(name: string): void { this.attributes.delete(name) }
  setAttributeNS(_ns: string, name: string, value: unknown): void { this.setAttribute(name, value) }
  removeAttributeNS(_ns: string, name: string): void { this.removeAttribute(name) }

  // The DOM properties Vue sets as properties, and a spec reads back.
  get className(): string { return this.getAttribute('class') ?? '' }
  set className(value: string) { this.setAttribute('class', value) }
  get id(): string { return this.getAttribute('id') ?? '' }
  set id(value: string) { this.setAttribute('id', value) }
  get disabled(): boolean { return this.hasAttribute('disabled') }
  set disabled(value: boolean) { if (value) this.setAttribute('disabled', ''); else this.removeAttribute('disabled') }
  get checked(): boolean { return this.hasAttribute('checked') }
  set checked(value: boolean) { if (value) this.setAttribute('checked', ''); else this.removeAttribute('checked') }
  get value(): string { return this.field }
  set value(value: string) { this.field = String(value) }
  get type(): string { return this.getAttribute('type') ?? '' }
  set type(value: string) { this.setAttribute('type', value) }
  get open(): boolean { return this.hasAttribute('open') }
  set open(value: boolean) { if (value) this.setAttribute('open', ''); else this.removeAttribute('open') }
  // Enough of <select> for v-model to mark the chosen option, and for a spec to read which it is.
  selectedIndex = -1
  get options(): FakeElement[] { return this.querySelectorAll('option') }
  get multiple(): boolean { return this.hasAttribute('multiple') }
  get selected(): boolean {
    const select = this.closest('select')
    return Boolean(select && select.options.indexOf(this) === select.selectedIndex)
  }
  get isContentEditable(): boolean { return false }

  focus(): void { fakeDocument.activeElement = this }
  blur(): void { if (fakeDocument.activeElement === this) fakeDocument.activeElement = fakeDocument.body }
  showModal(): void { this.setAttribute('open', '') }
  close(): void { this.removeAttribute('open') }
  scrollTo(): void {}
  scrollIntoView(): void {}
  /** One box when on the page and not hidden with display: none (v-show), else none. */
  getClientRects(): object[] {
    if (!this.isConnected) return []
    for (let node: FakeNode | null = this; node instanceof FakeElement; node = node.parentNode) if (node.style.display === 'none') return []
    return [{}]
  }
  getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 } }

  matches(selector: string): boolean { return selector.split(',').some(one => matchesChain(this, one.trim().split(/\s+/))) }
  closest(selector: string): FakeElement | null {
    for (let node: FakeNode | null = this; node instanceof FakeElement; node = node.parentNode) if (node.matches(selector)) return node
    return null
  }
  querySelectorAll(selector: string): FakeElement[] {
    const found: FakeElement[] = []
    const walk = (node: FakeNode) => {
      for (const child of node.childNodes) {
        if (child instanceof FakeElement) {
          if (child.matches(selector)) found.push(child)
          walk(child)
        }
      }
    }
    walk(this)
    return found
  }
  querySelector(selector: string): FakeElement | null { return this.querySelectorAll(selector)[0] ?? null }
}

class FakeSVGElement extends FakeElement {}

/** One compound selector: tag, #id, .class, [attr] / [attr="value"] and :not(one of those), in any mix. */
function matchesOne(element: FakeElement, selector: string): boolean {
  const parts = selector.match(/^[a-z][\w-]*|#[\w-]+|\.[\w-]+|\[[^\]]+\]|:not\([^()]+\)/gi) ?? []
  if (parts.join('') !== selector) throw new Error(`test/dom.ts cannot read the selector ${selector}`)
  return parts.every(part => {
    if (part.startsWith(':not(')) return !matchesOne(element, part.slice(5, -1))
    if (part[0] === '#') return element.id === part.slice(1)
    if (part[0] === '.') return element.className.split(/\s+/).includes(part.slice(1))
    if (part[0] === '[') {
      const [, name = '', value] = /^\[([^=\]]+)(?:="?([^"\]]*)"?)?\]$/.exec(part) ?? []
      return value === undefined ? element.hasAttribute(name) : element.getAttribute(name) === value
    }
    return element.tagName === part.toUpperCase()
  })
}

/** A descendant chain ("dialog .alert"): the last matches here, the ones before on ancestors, in order. */
function matchesChain(element: FakeElement, chain: string[]): boolean {
  if (!matchesOne(element, chain.at(-1)!)) return false
  let rest = chain.slice(0, -1)
  for (let node = element.parentNode; node instanceof FakeElement && rest.length; node = node.parentNode) {
    if (matchesOne(node, rest.at(-1)!)) rest = rest.slice(0, -1)
  }
  return rest.length === 0
}

class FakeDocument extends FakeNode {
  readonly documentElement = new FakeElement('html')
  readonly head = new FakeElement('head')
  readonly body = new FakeElement('body')
  activeElement: FakeElement | null = null
  constructor() {
    super(9, '#document')
    this.appendChild(this.documentElement)
    this.documentElement.appendChild(this.head)
    this.documentElement.appendChild(this.body)
    this.activeElement = this.body
  }
  createElement(tag: string): FakeElement { return new FakeElement(tag) }
  createElementNS(namespace: string, tag: string): FakeElement { return namespace.endsWith('/svg') ? new FakeSVGElement(tag, namespace) : new FakeElement(tag, namespace) }
  createTextNode(text: string): FakeText { return new FakeText(text) }
  createComment(text: string): FakeComment { return new FakeComment(text) }
  getElementById(id: string): FakeElement | null { return this.documentElement.querySelector(`#${id}`) }
  querySelector(selector: string): FakeElement | null { return this.documentElement.matches(selector) ? this.documentElement : this.documentElement.querySelector(selector) }
  querySelectorAll(selector: string): FakeElement[] { return this.documentElement.querySelectorAll(selector) }
}

class FakeShadowRoot {}

const fakeDocument = new FakeDocument()
const fakeWindow = {
  innerWidth: 1280, innerHeight: 900, ShadowRoot: FakeShadowRoot, trustedTypes: undefined,
  getComputedStyle: () => ({ transitionDuration: '', animationDuration: '' }),
  matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
  addEventListener() {}, removeEventListener() {},
}
Object.assign(globalThis, {
  document: fakeDocument, window: fakeWindow, Node: FakeNode, Element: FakeElement, HTMLElement: FakeElement, SVGElement: FakeSVGElement,
  Document: FakeDocument, ShadowRoot: FakeShadowRoot,
})

export type { FakeElement }

function event(type: string, target: FakeNode, fields: Partial<FakeEvent> = {}): FakeEvent {
  return {
    type, target, currentTarget: null, relatedTarget: null, defaultPrevented: false,
    preventDefault() { this.defaultPrevented = true }, stopPropagation() {}, stopImmediatePropagation() {}, ...fields,
  }
}

/** Lets pending promises settle and Vue draw what they changed. */
export async function flush(rounds = 5): Promise<void> {
  for (let i = 0; i < rounds; i++) await new Promise(resolve => setImmediate(resolve))
}

/** Presses a button (or anything clickable): a click, unless it is disabled. Then lets Vue draw. */
export async function click(element: FakeElement | null): Promise<void> {
  if (!element) throw new Error('nothing to click')
  if (element.disabled) throw new Error(`the ${element.tagName.toLowerCase()} is disabled`)
  element.dispatchEvent(event('click', element))
  await flush()
}

/** Types into a field: its value, then the input event v-model listens to. Then lets Vue draw. */
export async function fill(element: FakeElement | null, value: string): Promise<void> {
  if (!element) throw new Error('nothing to fill')
  if (element.disabled) throw new Error(`the ${element.tagName.toLowerCase()} is disabled`)
  element.value = value
  element.dispatchEvent(event('input', element))
  await flush()
}

/** Ticks a checkbox or picks a radio button: checked, then the change event v-model listens to. */
export async function check(element: FakeElement | null, checked = true): Promise<void> {
  if (!element) throw new Error('nothing to check')
  if (element.disabled) throw new Error(`the ${element.tagName.toLowerCase()} is disabled`)
  element.checked = checked
  element.dispatchEvent(event('change', element))
  await flush()
}

/** Submits a form, as Enter in a field or its submit button would. */
export async function submit(form: FakeElement | null): Promise<void> {
  if (!form) throw new Error('nothing to submit')
  form.dispatchEvent(event('submit', form))
  await flush()
}

/** A key pressed where the focus is, as the browser sends it: to the element, bubbling to the document. */
export function keydown(fields: Partial<FakeEvent> & { key: string }): FakeEvent {
  const target = fakeDocument.activeElement ?? fakeDocument.body
  const pressed = event('keydown', target, fields)
  target.dispatchEvent(pressed)
  return pressed
}

/** Any other event, as the browser sends it to this element (the cancel a modal dialog gets on Escape, a pointerdown). Then lets Vue draw. */
export async function fire(element: FakeElement | null, type: string, fields: Partial<FakeEvent> = {}): Promise<FakeEvent> {
  if (!element) throw new Error(`nothing to send ${type} to`)
  const sent = event(type, element, fields)
  element.dispatchEvent(sent)
  await flush()
  return sent
}

/** The page's text, as a person reads it: whitespace folded. */
export function words(element: FakeNode = fakeDocument.body): string {
  return element.textContent.replace(/\s+/g, ' ').trim()
}

/** The first element matching a selector whose text contains these words. */
export function find(selector: string, text = ''): FakeElement | null {
  return fakeDocument.querySelectorAll(selector).find(element => words(element).includes(text)) ?? null
}

export { fakeDocument as page }
