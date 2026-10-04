// A Vitest environment for specs that mount components on test/dom.ts's page:
// Node, like the other specs, but with modules transformed for the browser,
// so a .vue file compiles to what mounts on a page rather than to what
// renders on a server. It installs nothing; the spec imports test/dom.ts
// first. Specs named *.page.spec.ts run in it (vite.config.ts).
import type { Environment } from 'vitest/environments'

export default {
  name: 'client-in-node',
  viteEnvironment: 'client',
  setup: () => ({ teardown() {} }),
} satisfies Environment
