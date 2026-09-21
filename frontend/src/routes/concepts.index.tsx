import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate, useSearch } from '@tanstack/react-router'
import type { ConceptPage, GrepHit, HitOut } from '../client'
import { grepGrepGet, listConceptsConceptsGet, searchSearchGet } from '../client'
import { PathTree } from '../components/PathTree'
import { SearchBox } from '../components/SearchBox'
import { TenantSwitcher } from '../components/TenantSwitcher'

const PAGE_SIZE = 50
// There is no "list distinct prefixes" endpoint, so the tree samples the
// largest page the API allows and is incomplete beyond that many concepts.
const TREE_SAMPLE_SIZE = 200

export const Route = createFileRoute('/concepts/')({
  validateSearch: (search: Record<string, unknown>): { prefix?: string; q?: string; offset?: number } => ({
    prefix: typeof search.prefix === 'string' ? search.prefix : undefined,
    q: typeof search.q === 'string' ? search.q : undefined,
    offset: typeof search.offset === 'number' ? search.offset : undefined,
  }),
  component: Browse,
})

type Mode = 'browse' | 'search' | 'grep'

function modeOf(q: string): Mode {
  if (q === '') return 'browse'
  return q.startsWith('/') ? 'grep' : 'search'
}

function Browse() {
  const { tenant } = useSearch({ strict: false })
  const { prefix = '', q = '', offset = 0 } = Route.useSearch()
  const navigate = useNavigate()
  const mode = modeOf(q)
  const showTenant = tenant === undefined

  // A page deep-link is shareable like prefix/q/tenant, so it lives in the
  // URL too. Changing the filter starts back at page one.
  const setPrefix = (next: string) =>
    void navigate({ to: '.', search: (prev) => ({ ...prev, prefix: next || undefined, offset: undefined }) })
  const setQuery = (next: string) =>
    void navigate({ to: '.', search: (prev) => ({ ...prev, q: next || undefined, offset: undefined }) })
  const setOffset = (next: number) =>
    void navigate({ to: '.', search: (prev) => ({ ...prev, offset: next || undefined }) })

  const tree = useQuery({
    queryKey: ['concepts-tree', tenant],
    queryFn: async () =>
      (await listConceptsConceptsGet({ query: { tenant, limit: TREE_SAMPLE_SIZE }, throwOnError: true }))
        .data,
  })

  const page = useQuery({
    queryKey: ['concepts-page', tenant, prefix, offset],
    queryFn: async () =>
      (
        await listConceptsConceptsGet({
          query: { tenant, prefix, limit: PAGE_SIZE, offset },
          throwOnError: true,
        })
      ).data,
    enabled: mode === 'browse',
  })

  // search/grep require a concrete tenant -- the `!` is safe because `enabled`
  // below keeps the query from running until `tenant` is set.
  const searchResults = useQuery({
    queryKey: ['concepts-search', tenant, q],
    queryFn: async () =>
      (await searchSearchGet({ query: { tenant: tenant!, q, limit: PAGE_SIZE }, throwOnError: true }))
        .data,
    enabled: mode === 'search' && !!tenant,
  })

  const grepResults = useQuery({
    queryKey: ['concepts-grep', tenant, q],
    queryFn: async () =>
      (
        await grepGrepGet({
          query: { tenant: tenant!, pattern: q.slice(1), limit: PAGE_SIZE },
          throwOnError: true,
        })
      ).data,
    enabled: mode === 'grep' && !!tenant,
  })

  return (
    <div className="flex gap-6 p-4">
      <div className="w-56 flex-shrink-0">
        <TenantSwitcher />
        <div className="mt-4">
          <PathTree
            paths={tree.data?.items.map((item) => item.path)}
            selectedPrefix={prefix}
            onSelect={setPrefix}
          />
          {tree.data && tree.data.total > TREE_SAMPLE_SIZE && (
            <p className="mt-2 text-xs text-gray-500">
              Showing the first {TREE_SAMPLE_SIZE} of {tree.data.total} concepts.
            </p>
          )}
        </div>
      </div>

      <div className="min-w-0 flex-1">
        <SearchBox value={q} onChange={setQuery} />

        {mode !== 'browse' && !tenant && (
          <p className="mt-3 text-sm text-gray-500">Select a tenant to search or grep.</p>
        )}

        {mode === 'browse' && (
          <BrowseTable
            page={page.data}
            isLoading={page.isLoading}
            offset={offset}
            onOffset={setOffset}
            showTenant={showTenant}
          />
        )}
        {mode === 'search' && tenant && (
          <SearchTable hits={searchResults.data} isLoading={searchResults.isLoading} tenant={tenant} />
        )}
        {mode === 'grep' && tenant && (
          <GrepTable hits={grepResults.data} isLoading={grepResults.isLoading} tenant={tenant} />
        )}
      </div>
    </div>
  )
}

function BrowseTable({
  page,
  isLoading,
  offset,
  onOffset,
  showTenant,
}: {
  page: ConceptPage | undefined
  isLoading: boolean
  offset: number
  onOffset: (offset: number) => void
  showTenant: boolean
}) {
  if (isLoading) return <div className="mt-3 h-48 animate-pulse rounded border bg-gray-100" />
  if (!page || page.items.length === 0) {
    return <p className="mt-3 text-sm text-gray-500">No concepts.</p>
  }
  return (
    <div className="mt-3">
      <table className="w-full text-sm">
        <thead>
          <tr className="text-left text-gray-500">
            <th className="py-1 pr-2 font-medium">Path</th>
            <th className="py-1 pr-2 font-medium">Type</th>
            <th className="py-1 pr-2 font-medium">Title</th>
            {showTenant && <th className="py-1 pr-2 font-medium">Tenant</th>}
            <th className="py-1 pr-2 font-medium">Version</th>
            <th className="py-1 font-medium">Updated</th>
          </tr>
        </thead>
        <tbody>
          {page.items.map((item) => (
            <tr key={`${item.tenant_id}:${item.path}`} className="border-t">
              <td className="py-1 pr-2 font-mono">
                <Link
                  to="/concepts/$"
                  params={{ _splat: item.path }}
                  search={{ tenant: item.tenant_id }}
                  className="text-blue-600 hover:underline"
                >
                  {item.path}
                </Link>
              </td>
              <td className="py-1 pr-2">{item.type}</td>
              <td className="py-1 pr-2">{item.title}</td>
              {showTenant && <td className="py-1 pr-2 font-mono">{item.tenant_id}</td>}
              <td className="py-1 pr-2">{item.version}</td>
              <td className="py-1">{new Date(item.updated_at).toLocaleString()}</td>
            </tr>
          ))}
        </tbody>
      </table>
      <div className="mt-2 flex items-center gap-3 text-sm text-gray-500">
        <button
          disabled={offset === 0}
          onClick={() => onOffset(Math.max(0, offset - PAGE_SIZE))}
          className="rounded border px-2 py-1 disabled:opacity-50"
        >
          Prev
        </button>
        <span>
          {offset + 1}-{Math.min(offset + PAGE_SIZE, page.total)} of {page.total}
        </span>
        <button
          disabled={offset + PAGE_SIZE >= page.total}
          onClick={() => onOffset(offset + PAGE_SIZE)}
          className="rounded border px-2 py-1 disabled:opacity-50"
        >
          Next
        </button>
      </div>
    </div>
  )
}

function SearchTable({
  hits,
  isLoading,
  tenant,
}: {
  hits: HitOut[] | undefined
  isLoading: boolean
  tenant: string
}) {
  if (isLoading) return <div className="mt-3 h-48 animate-pulse rounded border bg-gray-100" />
  if (!hits || hits.length === 0) return <p className="mt-3 text-sm text-gray-500">No matches.</p>
  return (
    <table className="mt-3 w-full text-sm">
      <thead>
        <tr className="text-left text-gray-500">
          <th className="py-1 pr-2 font-medium">Path</th>
          <th className="py-1 pr-2 font-medium">Type</th>
          <th className="py-1 pr-2 font-medium">Title</th>
          <th className="py-1 pr-2 font-medium">Description</th>
          <th className="py-1 font-medium">Score</th>
        </tr>
      </thead>
      <tbody>
        {hits.map((hit) => (
          <tr key={hit.path} className="border-t">
            <td className="py-1 pr-2 font-mono">
              <Link
                to="/concepts/$"
                params={{ _splat: hit.path }}
                search={{ tenant }}
                className="text-blue-600 hover:underline"
              >
                {hit.path}
              </Link>
            </td>
            <td className="py-1 pr-2">{hit.type}</td>
            <td className="py-1 pr-2">{hit.title}</td>
            <td className="py-1 pr-2 text-gray-500">{hit.description}</td>
            <td className="py-1">{hit.score.toFixed(2)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function GrepTable({
  hits,
  isLoading,
  tenant,
}: {
  hits: GrepHit[] | undefined
  isLoading: boolean
  tenant: string
}) {
  if (isLoading) return <div className="mt-3 h-48 animate-pulse rounded border bg-gray-100" />
  if (!hits || hits.length === 0) return <p className="mt-3 text-sm text-gray-500">No matches.</p>
  return (
    <table className="mt-3 w-full text-sm">
      <thead>
        <tr className="text-left text-gray-500">
          <th className="py-1 pr-2 font-medium">Path</th>
          <th className="py-1 font-medium">Snippet</th>
        </tr>
      </thead>
      <tbody>
        {hits.map((hit) => (
          <tr key={hit.path} className="border-t">
            <td className="py-1 pr-2 whitespace-nowrap font-mono">
              <Link
                to="/concepts/$"
                params={{ _splat: hit.path }}
                search={{ tenant }}
                className="text-blue-600 hover:underline"
              >
                {hit.path}
              </Link>
            </td>
            <td className="py-1 font-mono text-gray-500">{hit.snippet}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
