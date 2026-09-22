import { expect, test, type Page } from '@playwright/test'
import { TENANT_A } from './fixture-constants'

// Runs in the 'screenshots' project (frontend/playwright.config.ts): authenticated
// via storageState, 1440x900 by default, UTC/en-US so toLocaleString() renders the
// same in whatever environment checks a baseline as in the one that generated it.
//
// Every seeded timestamp is pinned (scripts/ui_fixture.py's _freeze_timestamps), so
// the ambient clock is the only remaining source of drift. Frozen here too -- even
// though nothing in the app reads it directly today -- so a future relative-time
// display doesn't quietly reintroduce the flake this suite exists to avoid.
const FROZEN_TIME = '2024-01-15T12:00:00Z'
const MOBILE_VIEWPORT = { width: 390, height: 844 }

// Each call is two assertions with different jobs: `page.screenshot()` writes a
// picture for a person to look at, `toHaveScreenshot()` diffs against a committed
// baseline. Neither substitutes for the other.
async function capture(page: Page, name: string) {
  await page.screenshot({ path: `screenshots/${name}.png` })
  await expect(page).toHaveScreenshot(`${name}.png`)
}

function statValue(page: Page, label: string) {
  return page.getByText(label, { exact: true }).locator('xpath=following-sibling::div[1]')
}

test.beforeEach(async ({ page }) => {
  await page.clock.setFixedTime(FROZEN_TIME)
})

test('login', async ({ page }) => {
  await page.goto('/login')
  await expect(page.getByRole('button', { name: 'Log in' })).toBeVisible()
  await capture(page, 'login')
})

test('overview, all tenants', async ({ page }) => {
  await page.goto('/')
  await expect(statValue(page, 'Concepts')).toHaveText('24')
  await capture(page, 'overview-all-tenants')
})

test('overview, one tenant', async ({ page }) => {
  await page.goto(`/?tenant=${TENANT_A}`)
  await expect(statValue(page, 'Concepts')).toHaveText('12')
  await capture(page, 'overview-one-tenant')
})

test('browse', async ({ page }) => {
  await page.goto(`/concepts?tenant=${TENANT_A}`)
  await expect(page.getByRole('link', { name: 'auth/login', exact: true })).toBeVisible()
  await capture(page, 'browse')
})

test('browse, search results', async ({ page }) => {
  await page.goto(`/concepts?tenant=${TENANT_A}`)
  await page.getByPlaceholder('Search, or /pattern to grep').fill('oncall')
  await expect(page.getByRole('link', { name: 'ops/oncall', exact: true })).toBeVisible()
  await capture(page, 'browse-search')
})

test('concept detail', async ({ page }) => {
  await page.goto(`/concepts/auth/login?tenant=${TENANT_A}`)
  await expect(page.getByRole('heading', { level: 1, name: 'Login' })).toBeVisible()
  await capture(page, 'detail')
})

test('concept detail, scrolled to revisions', async ({ page }) => {
  await page.goto(`/concepts/auth/login?tenant=${TENANT_A}`)
  // auth/login is the one seeded concept with a v2 (scripts/ui_fixture.py), so its
  // revision table is the one worth a screenshot.
  await page.getByRole('heading', { name: 'Revision history' }).scrollIntoViewIfNeeded()
  await expect(page.getByText('update', { exact: true })).toBeVisible()
  await capture(page, 'detail-revisions')
})

test('overview, mobile', async ({ page }) => {
  await page.setViewportSize(MOBILE_VIEWPORT)
  await page.goto('/')
  await expect(statValue(page, 'Concepts')).toHaveText('24')
  await capture(page, 'overview-mobile')
})
