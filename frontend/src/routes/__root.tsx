import { createRootRoute, Link, Outlet, useRouterState } from '@tanstack/react-router'
import { logoutSessionDelete } from '../client'

export const Route = createRootRoute({
  component: RootLayout,
  // Absent tenant means "all tenants" everywhere the search param is read.
  validateSearch: (search: Record<string, unknown>): { tenant?: string } => ({
    tenant: typeof search.tenant === 'string' ? search.tenant : undefined,
  }),
})

async function logout() {
  // Best-effort: an already-expired session 401s here, but the destination is
  // /login either way, so the response isn't worth branching on.
  await logoutSessionDelete()
  window.location.assign('/login')
}

function RootLayout() {
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  // /login is the one unauthenticated route -- it has nothing to navigate to yet.
  if (pathname === '/login') return <Outlet />

  return (
    <div className="flex min-h-screen flex-col">
      <nav className="flex items-center gap-4 border-b px-4 py-2 text-sm">
        <Link to="/" activeOptions={{ exact: true }} activeProps={{ className: 'font-semibold' }}>
          Overview
        </Link>
        <Link to="/concepts" activeProps={{ className: 'font-semibold' }}>
          Browse
        </Link>
        <button type="button" onClick={logout} className="ml-auto text-gray-500 hover:underline">
          Log out
        </button>
      </nav>
      <Outlet />
    </div>
  )
}
