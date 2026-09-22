import { createServer } from 'node:net'
import { defineConfig, devices } from '@playwright/test'

// Playwright's webServer.url must be known before the command starts (it polls
// that URL for readiness), so the port is picked here and handed to the fixture
// script via an env var rather than letting the script pick its own.
//
// Each test worker is a separate process that re-imports this file, so picking
// the port fresh every time would give every worker a different, unlistened-on
// port. Caching it in process.env survives that: the root process picks it once
// and workers, forked after webServer is already up, inherit the same value.
async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const server = createServer()
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      if (address === null || typeof address === 'string') {
        reject(new Error('could not allocate a free port'))
        return
      }
      const { port } = address
      server.close(() => resolve(port))
    })
    server.on('error', reject)
  })
}

process.env.UI_FIXTURE_PORT ??= String(await freePort())
const PORT = Number(process.env.UI_FIXTURE_PORT)
const BASE_URL = `http://127.0.0.1:${PORT}`

export default defineConfig({
  testDir: './tests',
  fullyParallel: false,
  retries: 0,
  use: {
    baseURL: BASE_URL,
    trace: 'retain-on-failure',
  },
  webServer: {
    command: '../scripts/ui-fixture.sh',
    url: `${BASE_URL}/readyz`,
    env: { UI_FIXTURE_PORT: String(PORT) },
    timeout: 180_000,
    reuseExistingServer: false,
  },
  projects: [
    { name: 'setup', testMatch: /auth\.setup\.ts/, use: { ...devices['Desktop Chrome'] } },
    { name: 'auth-specs', testMatch: /auth\.spec\.ts/, use: { ...devices['Desktop Chrome'] } },
    {
      name: 'console-specs',
      testMatch: /console\.spec\.ts/,
      use: { ...devices['Desktop Chrome'], storageState: 'tests/.auth/state.json' },
      dependencies: ['setup'],
    },
  ],
})
