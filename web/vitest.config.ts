import { defineConfig } from 'vitest/config'
import vue from '@vitejs/plugin-vue'

// src/ is the core the editions build on: each compiles it from source through
// an alias of its own, in a project of its own, which serves and builds the
// page. Nothing here builds or serves one: a self-hosted server has no web
// console (platform decision 0063). This project only checks the core: its
// types (npm run typecheck) and its specs (npm test), under the specs' own
// edition (test/edition.ts). Nothing here imports an edition, nor needs an
// alias to reach itself.
export default defineConfig({
  plugins: [vue()],
  test: {
    projects: [
      // Each spec runs with the specs' edition configured (test/setup.ts), as
      // an edition's entry does before mounting.
      { extends: true, test: { name: 'node', environment: 'node', include: ['test/**/*.spec.ts'], exclude: ['test/**/*.page.spec.ts'], setupFiles: ['./test/setup.ts'] } },
      // Components mounted on a small fake page (test/dom.ts) and pressed:
      // still Node, with the modules built for the browser.
      { extends: true, test: { name: 'page', environment: './test/client.env.ts', include: ['test/**/*.page.spec.ts'], setupFiles: ['./test/setup.page.ts'] } },
    ],
  },
})
