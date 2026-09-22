import { test as setup } from '@playwright/test'
import { ADMIN_PASSWORD } from './fixture-constants'

const STORAGE_STATE = 'tests/.auth/state.json'

setup('log in', async ({ page }) => {
  await page.goto('/login')
  await page.getByPlaceholder('Password').fill(ADMIN_PASSWORD)
  await page.getByRole('button', { name: 'Log in' }).click()
  await page.waitForURL('**/')
  await page.context().storageState({ path: STORAGE_STATE })
})
