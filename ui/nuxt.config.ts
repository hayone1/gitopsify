// The web UI is a single-page app: gitopsify serves the generated files and
// its API from the same origin (no server-side rendering, no Node at runtime).
export default defineNuxtConfig({
  modules: ['@nuxt/ui'],
  ssr: false,
  devtools: { enabled: false },
  css: ['~/assets/css/main.css'],
  compatibilityDate: '2026-11-01',
  app: {
    head: {
      title: 'gitopsify',
      htmlAttrs: { lang: 'en' },
      link: [{ rel: 'icon', type: 'image/svg+xml', href: '/favicon.svg' }],
      meta: [{ name: 'viewport', content: 'width=device-width, initial-scale=1' }]
    }
  },
  // Air-gapped and CSP-friendly: icons are bundled, no web fonts are fetched.
  ui: { fonts: false },
  icon: {
    provider: 'none',
    clientBundle: { scan: true, sizeLimitKb: 1024 }
  },
  // pnpm dev talks to a gitopsify server on :8080 (gitopsify ui --listen 127.0.0.1:8080).
  nitro: {
    devProxy: { '/api': { target: 'http://127.0.0.1:8080/api', changeOrigin: true } }
  },
  typescript: { strict: true }
})
