import tailwindcss from '@tailwindcss/vite'
import { tanstackRouter } from '@tanstack/router-plugin/vite'
import react from '@vitejs/plugin-react'
import { configDefaults, defineConfig } from 'vitest/config'

// Same-origin proxy in dev: the session cookie is SameSite=Strict, so a
// cross-origin dev server would never see it come back.
export default defineConfig({
  base: '/',
  plugins: [tanstackRouter({ target: 'react', autoCodeSplitting: true }), react(), tailwindcss()],
  build: {
    outDir: 'dist',
  },
  server: {
    proxy: {
      '/api': 'http://localhost:8000',
      '/mcp': 'http://localhost:8000',
    },
  },
  test: {
    // `tests/` holds Playwright specs (their own `test` fixture, not vitest's) --
    // exclude them so `bun run test` doesn't try to collect them.
    exclude: [...configDefaults.exclude, 'tests/**'],
  },
})
