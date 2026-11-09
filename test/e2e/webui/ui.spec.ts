import { execFileSync, execSync, spawn } from 'node:child_process'
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
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
// shot saves the page in the light and the dark theme (the color mode follows
// the system until someone picks one).
async function shot(page: Page, name: string) {
  await page.screenshot({ path: `screenshots/${name}.png`, fullPage: true, animations: 'disabled' })
  await page.emulateMedia({ colorScheme: 'dark' })
  await expect(page.locator('html')).toHaveClass(/dark/)
  await page.screenshot({ path: `screenshots/${name}-dark.png`, fullPage: true, animations: 'disabled' })
  await page.emulateMedia({ colorScheme: 'light' })
  await expect(page.locator('html')).not.toHaveClass(/dark/)
}

test('an operator scans, generates and pushes a migration', async ({ page }) => {
  // Uncaught errors in the page fail the test.
  const errors: string[] = []
  page.on('pageerror', err => errors.push(err.message))
  await signIn(page, 'ada')
  await shot(page, '01-overview')

  // From the Clusters page's Migrate button: the cluster is preselected and
  // its namespaces are asked for (listing them may be forbidden to ada).
  await page.goto('/clusters')
  const namespaces = page.waitForResponse(r => /\/api\/v1\/clusters\/[^/]+\/namespaces/.test(r.url()))
  await page.getByRole('link', { name: 'Migrate' }).first().click()
  await namespaces
  await expect(page.getByText('Source cluster')).toBeVisible()
  expect(errors).toEqual([])

  await page.goto('/')
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

  // The color mode: picked in the sidebar, kept across reloads.
  await page.getByRole('button', { name: 'Color mode' }).click()
  await page.getByRole('option', { name: 'Dark' }).click()
  await expect(page.locator('html')).toHaveClass(/dark/)
  await page.reload()
  await expect(page.locator('html')).toHaveClass(/dark/)
  await page.getByRole('button', { name: 'Color mode' }).click()
  await page.getByRole('option', { name: 'System' }).click()
  await expect(page.locator('html')).not.toHaveClass(/dark/)
})

// gitopsify login: the command line waits while the browser signs in.
const bin = join(__dirname, '../../../bin/gitopsify')
const server = process.env.BASE_URL || 'http://localhost:8080'

function startLogin(args: string[], env: NodeJS.ProcessEnv) {
  const p = spawn(bin, ['login', server, ...args], { env })
  let out = ''
  p.stdout.on('data', d => { out += d })
  p.stderr.on('data', d => { out += d })
  const done = new Promise<number>(resolve => p.on('exit', code => resolve(code ?? -1)))
  const url = new Promise<string>((resolve, reject) => {
    const timer = setInterval(() => {
      const m = out.match(/(https?:\/\/\S+\/api\/v1\/auth\/login\?\S+)/)
      if (m) {
        clearInterval(timer)
        resolve(m[1])
      }
    }, 100)
    void done.then(() => {
      clearInterval(timer)
      reject(new Error('gitopsify login ended early: ' + out))
    })
  })
  return { p, url, done, output: () => out }
}

test('gitopsify login signs the command line in through the browser', async ({ page }) => {
  const dir = mkdtempSync(join(tmpdir(), 'gitopsify-cli-'))
  const base = { ...process.env, GITOPSIFY_REMOTE_CONFIG: join(dir, 'servers.yaml'), GITOPSIFY_REMOTE: '', GITOPSIFY_REMOTE_TOKEN: '' }

  // Over SSH: the page shows a code to paste.
  const paste = startLogin(['--no-browser'], { ...base, DISPLAY: '', WAYLAND_DISPLAY: '' })
  await page.goto(await paste.url)
  await page.locator('#username').fill('ada')
  await page.locator('#password').fill('ada')
  await page.locator('#kc-login').click()
  // The page asks to confirm, with the check code the terminal shows.
  await expect(page.getByRole('heading', { name: 'Sign in on the command line' })).toBeVisible()
  const check = paste.output().match(/check code ([A-Z2-7]{4}-[A-Z2-7]{4})/)?.[1]
  expect(check, paste.output()).toBeTruthy()
  await expect(page.getByText(check!)).toBeVisible()
  await shot(page, '11-cli-confirm')
  await page.getByRole('button', { name: 'Sign in the command line' }).click()
  await expect(page.locator('code')).toBeVisible()
  await shot(page, '12-cli-code')
  const code = (await page.locator('code').textContent())?.trim() || ''
  expect(code.length).toBeGreaterThan(40)
  paste.p.stdin.write(code + '\n')
  expect(await paste.done, paste.output()).toBe(0)
  const me = JSON.parse(execFileSync(bin, ['whoami', '--format', 'json'], { env: base }).toString())
  expect(me.user.via).toBe('cli')
  expect(me.clusters.kind).toBe('operator')
  // The browser itself got no session.
  await page.goto('/')
  await expect(page).toHaveURL(/\/login/)

  // With a browser on this machine: it hands the code to the command line
  // on 127.0.0.1 (Keycloak remembers ada, so no form this time).
  const loop = startLogin(['--renew'], { ...base, DISPLAY: ':99' })
  await page.goto(await loop.url)
  await page.getByRole('button', { name: 'Sign in the command line' }).click()
  await expect(page.getByText('Signed in. You can close this tab')).toBeVisible()
  expect(await loop.done, loop.output()).toBe(0)
  execFileSync(bin, ['run', 'list'], { env: base })
  execFileSync(bin, ['logout'], { env: base })

  // A link someone else sent: canceling hands nothing over.
  const other = startLogin(['--no-browser'], { ...base, DISPLAY: '' })
  await page.goto(await other.url)
  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(page.getByText('Canceled')).toBeVisible()
  other.p.kill()
})
