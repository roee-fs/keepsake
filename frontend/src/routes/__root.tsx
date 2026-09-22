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
  // Best-effort: an expired session 401s here, but the destination is /login anyway.
  await logoutSessionDelete()
  window.location.assign('/login')
}

// Colour lives only in the active/inactive halves: TanStack concatenates them
// onto `className`, and two competing Tailwind colours would then resolve by
// CSS order rather than by which half applied.
const NAV_LINK = 'rounded-md px-2.5 py-1 transition-colors'
const NAV_ACTIVE = { className: 'bg-hover font-medium text-fg' }
const NAV_INACTIVE = { className: 'text-fg-muted hover:text-fg' }

function RootLayout() {
  const pathname = useRouterState({ select: (state) => state.location.pathname })
  // /login is the one unauthenticated route, with nothing to navigate to yet.
  if (pathname === '/login') return <Outlet />

  return (
    <div className="flex min-h-screen flex-col">
      <nav className="sticky top-0 z-10 flex items-center gap-1 border-b border-line bg-base px-4 py-2">
        <span className="mr-4 font-mono text-sm font-medium text-fg-faint select-none">keepsake</span>
        <Link
          to="/"
          activeOptions={{ exact: true }}
          className={NAV_LINK}
          activeProps={NAV_ACTIVE}
          inactiveProps={NAV_INACTIVE}
        >
          Overview
        </Link>
        <Link
          to="/concepts"
          className={NAV_LINK}
          activeProps={NAV_ACTIVE}
          inactiveProps={NAV_INACTIVE}
        >
          Browse
        </Link>
        <button
          type="button"
          onClick={logout}
          className="ml-auto rounded-md px-2.5 py-1 text-fg-muted transition-colors hover:bg-hover hover:text-fg"
        >
          Log out
        </button>
      </nav>
      <main className="mx-auto w-full max-w-[1200px] flex-1 px-6 py-6">
        <Outlet />
      </main>
    </div>
  )
}
