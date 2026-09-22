import { expect, test } from '@playwright/test'
import { ADMIN_PASSWORD, TENANT_A } from './fixture-constants'

// No storageState here -- these are the only specs that log in by hand.

test('a logged-out visit redirects to /login and returns after login', async ({ page }) => {
  // The app's own 401-triggered redirect (a hard `window.location.assign`) can
  // replace the /concepts document before Playwright finishes tracking its
  // navigation, which then reports the tracking itself as ERR_ABORTED. `goto`
  // tolerates that; `expect(...).toHaveURL` polls rather than tracking a single
  // navigation, so it isn't subject to the same race as `waitForURL`.
  await page.goto('/concepts').catch(() => {})
  await expect(page).toHaveURL(/\/login\?redirect=/)
  expect(page.url()).toContain(encodeURIComponent('/concepts'))

  await page.getByPlaceholder('Password').fill(ADMIN_PASSWORD)
  await page.getByRole('button', { name: 'Log in' }).click()
  await page.waitForURL('**/concepts')
})

test('a wrong password shows an error and does not navigate', async ({ page }) => {
  await page.goto('/login')
  await page.getByPlaceholder('Password').fill('not-the-password')
  await page.getByRole('button', { name: 'Log in' }).click()

  await expect(page.getByText('Incorrect password.')).toBeVisible()
  expect(page.url()).toContain('/login')
})

test('a deep-link reload returns the SPA shell, not a 404', async ({ page }) => {
  // The Task 12 catch-all: a path matching no static file must 200 with
  // index.html, not 404. Unauthenticated, so the app then bounces to /login --
  // that bounce is itself proof the shell booted and its JS ran.
  const response = await page.goto(`/concepts/auth/login?tenant=${TENANT_A}`)
  expect(response?.status()).toBe(200)
  await page.waitForURL(/\/login\?redirect=/)
})
