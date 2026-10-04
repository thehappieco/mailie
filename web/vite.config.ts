import { defineConfig } from 'vitest/config'
import type { Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import { hostedName } from './test/hosted.ts'

// No policy rewriting for development: index.html's style-src already allows
// inline styles, as the daemon's header does (internal/webui, which serves
// every edition), and that is also what Vite's dev server needs to inject its
// stylesheets. Dev and build serve the same policy.

// The console and the API share one origin (the browser sends the bearer token
// to 'self' only), so in development the API is reached through this proxy
// rather than at the daemon's own port, which would be cross-origin. The
// target is 127.0.0.1 because MAIL_HTTP_ADDR binds IPv4 only; changeOrigin
// stays false so the daemon sees the Host the browser used.
//
// src/ is also the core other editions build on: they compile it from source
// through an alias of their own. Nothing here imports one of them, nor needs
// an alias to reach itself.
// The open build names no hosted service, company or policy (test/hosted.ts):
// a file in the output with one fails the build, whatever brought it in. The
// sources are checked for the same names by test/editions.spec.ts.
function noHostedNames(): Plugin {
  return {
    name: 'mailie-no-hosted-names',
    apply: 'build',
    enforce: 'post',
    generateBundle(_, bundle) {
      const found = Object.values(bundle).flatMap(file => {
        const text = file.type === 'chunk' ? file.code : typeof file.source === 'string' ? file.source : new TextDecoder().decode(file.source)
        const name = hostedName(text)
        return name ? [`${file.fileName} (${name})`] : []
      })
      if (found.length) this.error(`the open console names a hosted service: ${found.join(', ')}`)
    },
  }
}

export default defineConfig({
  plugins: [vue(), noHostedNames()],
  server: {
    // 5174, not Vite's 5173: the Wappie console already takes that port, and
    // both are run side by side. strictPort, because the OAuth web client's
    // registered redirect names this exact port. Another edition's dev server
    // may use the same port for the same reason; only one runs at a time.
    port: 5174,
    strictPort: true,
    // /mcp too, so the MCP URL the API & MCP section shows in development
    // (this origin's /mcp, as on a daemon serving the console itself) reaches
    // the daemon. An MCP client sends no Origin; the daemon still checks one
    // that is present.
    proxy: {
      '/v1': {
        target: process.env.MAIL_DEV_TARGET ?? 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
      '^/mcp(\\?|$)': {
        target: process.env.MAIL_DEV_TARGET ?? 'http://127.0.0.1:8080',
        changeOrigin: false,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
  },
  test: {
    projects: [
      // Each spec runs with the open edition configured (test/setup.ts), as
      // src/main.ts does before mounting.
      { extends: true, test: { name: 'node', environment: 'node', include: ['test/**/*.spec.ts'], exclude: ['test/**/*.page.spec.ts'], setupFiles: ['./test/setup.ts'] } },
      // Components mounted on a small fake page (test/dom.ts) and pressed:
      // still Node, with the modules built for the browser.
      { extends: true, test: { name: 'page', environment: './test/client.env.ts', include: ['test/**/*.page.spec.ts'], setupFiles: ['./test/setup.page.ts'] } },
    ],
  },
})
