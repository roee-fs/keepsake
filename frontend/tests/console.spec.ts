import { expect, test } from '@playwright/test'
import { TENANT_A } from './fixture-constants'

// Authenticated via storageState (frontend/playwright.config.ts's 'setup' project).

function statValue(page: import('@playwright/test').Page, label: string) {
  return page.getByText(label, { exact: true }).locator('xpath=following-sibling::div[1]')
}

test('overview shows the seeded totals and the tenant switcher narrows them', async ({ page }) => {
  await page.goto('/')

  await expect(statValue(page, 'Concepts')).toHaveText('24')
  await expect(statValue(page, 'Types')).toHaveText('4')
  await expect(statValue(page, 'Revisions')).toHaveText('25')
  await expect(statValue(page, 'Orphans')).toHaveText('8')

  await page.locator('select').selectOption(TENANT_A)
  await expect(statValue(page, 'Concepts')).toHaveText('12')
  await expect(statValue(page, 'Revisions')).toHaveText('13')
  await expect(statValue(page, 'Orphans')).toHaveText('4')

  await page.locator('select').selectOption('')
  await expect(statValue(page, 'Concepts')).toHaveText('24')
})

test('the path tree filters the browse table', async ({ page }) => {
  await page.goto(`/concepts?tenant=${TENANT_A}`)

  const authNode = page.getByRole('button', { name: 'auth (5)' })
  await expect(authNode).toBeVisible()
  await authNode.click()

  await expect(page).toHaveURL(/prefix=auth%2F/)
  await expect(page.getByRole('link', { name: 'auth/login', exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: 'billing/customer', exact: true })).toHaveCount(0)

  // Documented, not guessed: clicking a folder's label calls onSelect with
  // event.stopPropagation(), which blocks the native <summary> toggle, and the
  // <details> element's open state is computed once at mount (PathTree.tsx's
  // `initialOpen`). So a click-driven prefix change does not auto-expand nested
  // children -- "security" stays hidden even though the table above did filter.
  await expect(page.getByRole('button', { name: 'security (2)' })).not.toBeVisible()

  // A fresh load with the prefix already in the URL computes `initialOpen` from
  // it at mount, so this is where the tree does self-heal.
  await page.goto(`/concepts?tenant=${TENANT_A}&prefix=auth%2Fsecurity%2F`)
  await expect(page.getByRole('button', { name: 'security (2)' })).toBeVisible()
})

test('search narrows results, a row opens the concept, and a backlink navigates', async ({
  page,
}) => {
  await page.goto(`/concepts?tenant=${TENANT_A}`)

  await page.getByPlaceholder('Search, or /pattern to grep').fill('oncall')
  await expect(page.getByRole('link', { name: 'ops/oncall', exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: 'auth/login', exact: true })).toHaveCount(0)

  await page.getByRole('link', { name: 'ops/oncall', exact: true }).click()
  await expect(page.getByRole('heading', { level: 1, name: 'Oncall' })).toBeVisible()

  await page.goto(`/concepts/auth/security/mfa?tenant=${TENANT_A}`)
  await expect(page.getByRole('heading', { level: 1, name: 'MFA' })).toBeVisible()
  const linkedFrom = page.getByRole('heading', { name: 'Linked from' }).locator('xpath=following-sibling::*[1]')
  const backlink = linkedFrom.getByRole('link', { name: 'auth/login', exact: true })
  await expect(backlink).toBeVisible()
  await backlink.click()

  await expect(page.getByRole('heading', { level: 1, name: 'Login' })).toBeVisible()

  // A hard reload of a deep link, authenticated this time: the catch-all serves
  // the shell and the real content renders again, not just a blank page.
  await page.reload()
  await expect(page.getByRole('heading', { level: 1, name: 'Login' })).toBeVisible()
})
