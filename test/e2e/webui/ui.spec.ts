import { execSync } from 'node:child_process'
import { expect, test, type Page } from '@playwright/test'

// signIn goes through Keycloak's login form.
async function signIn(page: Page, user: string) {
  await page.goto('/')
  await expect(page).toHaveURL(/\/login/)
  await page.getByRole('button', { name: 'Sign in with single sign-on' }).click()
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(user)
  await page.locator('#kc-login').click()
  await expect(page.getByRole('heading', { name: 'Recent runs' })).toBeVisible()
}

const gitURL = process.env.GIT_URL || 'http://localhost:8099/gitops.git'
const shot = (page: Page, name: string) => page.screenshot({ path: `screenshots/${name}.png`, fullPage: true })

test('an operator scans, generates and pushes a migration', async ({ page }) => {
  await signIn(page, 'ada')
  await shot(page, '01-overview')

  await page.getByRole('link', { name: 'New migration' }).first().click()
  await expect(page.getByText('Source cluster')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Scan the cluster' })).toBeEnabled()
  await page.getByRole('button', { name: 'Scan the cluster' }).click()
  await expect(page.getByText(/apps selected/)).toBeVisible({ timeout: 5 * 60_000 })
  await shot(page, '02-apps')

  await page.getByRole('button', { name: 'Next' }).click()
  await page.getByPlaceholder('https://github.com/acme/platform-gitops.git').fill(gitURL)
  // Sealed Secrets (the default) needs a target cluster; the test has none.
  await expect(page.getByText('Sealing needs the controller')).toBeVisible()
  await expect(page.getByRole('button', { name: 'Review', disabled: false })).toHaveCount(0)
  await page.getByText('Skip', { exact: true }).click()
  await shot(page, '03-target')
  await page.getByRole('button', { name: 'Review', disabled: false }).click()
  await expect(page.getByText('Valid.')).toBeVisible()
  await page.getByRole('switch', { name: /Push when it's done/ }).click()
  await shot(page, '04-review')

  await page.getByRole('button', { name: 'Generate the repository' }).click()
  await expect(page).toHaveURL(/\/runs\/\d{8}-/)
  await expect(page.getByText('succeeded').first()).toBeVisible({ timeout: 8 * 60_000 })
  await shot(page, '05-run')

  await page.getByRole('tab', { name: 'Files' }).click()
  await expect(page.getByText('MIGRATION_REPORT.md').first()).toBeVisible()
  await shot(page, '06-files')
  await page.getByRole('tab', { name: /Pushes/ }).click()
  await expect(page.getByText(gitURL)).toBeVisible()
  await shot(page, '07-pushes')

  // The commit is on the Git server.
  const log = execSync(`git --git-dir=${process.env.GIT_DIR} log --format=%an%x09%s -1`).toString()
  expect(log).toContain('Ada Lovelace')
})

test('a viewer sees runs but can not start or push', async ({ page }) => {
  await signIn(page, 'bob')
  await expect(page.getByRole('link', { name: 'New migration' })).toHaveCount(0)
  await page.goto('/migrations/new')
  await expect(page.getByText("You can't start migrations")).toBeVisible()
  await page.goto('/runs?type=migrate')
  await page.locator('tbody a[href^="/runs/"]').first().click()
  await expect(page.getByText('succeeded').first()).toBeVisible()
  await expect(page.getByRole('button', { name: 'Push to Git' })).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'Download' })).toBeVisible()
  await shot(page, '08-viewer-run')
  await page.goto('/audit')
  await expect(page.getByText('Not allowed')).toBeVisible()
})

test('an admin reads the audit log', async ({ page }) => {
  await signIn(page, 'root')
  await page.getByRole('link', { name: 'Audit log' }).click()
  await expect(page.getByText('run.create').first()).toBeVisible()
  await expect(page.getByText('run.publish').first()).toBeVisible()
  await expect(page.getByText('auth.login').first()).toBeVisible()
  await shot(page, '09-audit')
  await page.getByRole('link', { name: 'Clusters' }).first().click()
  await expect(page.getByText(/Reachable, Kubernetes/).first()).toBeVisible()
  await shot(page, '10-clusters')
})
