import { defineConfig } from '@playwright/test'

export default defineConfig({
  testDir: '.',
  timeout: 10 * 60 * 1000,
  expect: { timeout: 30_000 },
  workers: 1,
  use: {
    baseURL: 'http://localhost:8080',
    viewport: { width: 1440, height: 900 },
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure'
  },
  outputDir: 'results'
})
