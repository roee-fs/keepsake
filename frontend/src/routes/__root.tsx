import { createRootRoute, Outlet } from '@tanstack/react-router'

export const Route = createRootRoute({
  component: Outlet,
  // Absent tenant means "all tenants" everywhere the search param is read.
  validateSearch: (search: Record<string, unknown>): { tenant?: string } => ({
    tenant: typeof search.tenant === 'string' ? search.tenant : undefined,
  }),
})
