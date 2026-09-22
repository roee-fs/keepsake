import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link, useNavigate, useSearch } from '@tanstack/react-router'
import type { ConceptPage, GrepHit, HitOut } from '../client'
import { grepGrepGet, listConceptsConceptsGet, searchSearchGet } from '../client'
import { PathTree } from '../components/PathTree'
import { SearchBox } from '../components/SearchBox'
import { TenantId } from '../components/TenantId'
import { TenantSwitcher } from '../components/TenantSwitcher'

const PAGE_SIZE = 50
// There is no "list distinct prefixes" endpoint, so the tree samples the
// largest page the API allows and is incomplete beyond that many concepts.
const TREE_SAMPLE_SIZE = 200

// A whole column of tinted paths would fight the row hover, so the link tint only
// appears under the cursor.
const PATH_LINK = 'text-fg transition-colors hover:text-link hover:underline'
const SKELETON = 'mt-3 h-48 animate-pulse rounded-md border border-line bg-surface'
const EMPTY = 'mt-3 text-fg-muted'
// The minimum keeps a one-row result from reading as a half-loaded page.
const TABLE_SHELL = 'min-h-96 overflow-x-auto rounded-md border border-line'

export const Route = createFileRoute('/concepts/')({
  validateSearch: (search: Record<string, unknown>): { prefix?: string; q?: string; offset?: number } => ({
    prefix: typeof search.prefix === 'string' ? search.prefix : undefined,
    q: typeof search.q === 'string' ? search.q : undefined,
    offset: typeof search.offset === 'number' ? search.offset : undefined,
  }),
  component: Browse,
})

export type Mode = 'browse' | 'search' | 'grep'

export function modeOf(q: string): Mode {
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

  // search/grep require a concrete tenant; the `!` is safe because `enabled` below
  // keeps the query from running until one is set.
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
    <div className="flex gap-8">
      <div className="w-56 flex-shrink-0">
        <TenantSwitcher />
        <div className="mt-4">
          <PathTree
            paths={tree.data?.items.map((item) => item.path)}
            selectedPrefix={prefix}
            onSelect={setPrefix}
          />
          {tree.data && tree.data.total > TREE_SAMPLE_SIZE && (
            <p className="mt-3 px-2 text-xs text-fg-faint">
              Showing the first {TREE_SAMPLE_SIZE} of {tree.data.total} concepts.
            </p>
          )}
        </div>
      </div>

      <div className="min-w-0 flex-1">
        <SearchBox value={q} onChange={setQuery} />

        {mode !== 'browse' && !tenant && (
          <p className={EMPTY}>Select a tenant to search or grep.</p>
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
  if (isLoading) return <div className={SKELETON} />
  if (!page || page.items.length === 0) {
    return <p className={EMPTY}>No concepts.</p>
  }
  return (
    <div className="mt-3">
      <div className={TABLE_SHELL}>
        <table className="tbl">
          <thead>
            <tr>
              <th>Path</th>
              <th>Type</th>
              <th>Title</th>
              {showTenant && <th>Tenant</th>}
              <th>Version</th>
              <th>Updated</th>
            </tr>
          </thead>
          <tbody>
            {page.items.map((item) => (
              <tr key={`${item.tenant_id}:${item.path}`}>
                <td className="font-mono whitespace-nowrap">
                  <Link
                    to="/concepts/$"
                    params={{ _splat: item.path }}
                    search={{ tenant: item.tenant_id }}
                    className={PATH_LINK}
                  >
                    {item.path}
                  </Link>
                </td>
                <td className="text-fg-muted">{item.type}</td>
                <td>{item.title}</td>
                {showTenant && (
                  <td>
                    <TenantId tenantId={item.tenant_id} />
                  </td>
                )}
                <td className="font-mono tabular-nums text-fg-muted">{item.version}</td>
                <td className="tabular-nums whitespace-nowrap text-fg-muted">
                  {new Date(item.updated_at).toLocaleString()}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="mt-3 flex items-center gap-3 text-fg-muted">
        <button
          disabled={offset === 0}
          onClick={() => onOffset(Math.max(0, offset - PAGE_SIZE))}
          className="rounded-md border border-line px-2 py-1 transition-colors hover:bg-hover hover:text-fg disabled:opacity-40 disabled:hover:bg-transparent"
        >
          Prev
        </button>
        <span className="tabular-nums">
          {offset + 1}-{Math.min(offset + PAGE_SIZE, page.total)} of {page.total}
        </span>
        <button
          disabled={offset + PAGE_SIZE >= page.total}
          onClick={() => onOffset(offset + PAGE_SIZE)}
          className="rounded-md border border-line px-2 py-1 transition-colors hover:bg-hover hover:text-fg disabled:opacity-40 disabled:hover:bg-transparent"
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
  if (isLoading) return <div className={SKELETON} />
  if (!hits || hits.length === 0) return <p className={EMPTY}>No matches.</p>
  return (
    <div className={`mt-3 ${TABLE_SHELL}`}>
      <table className="tbl">
        <thead>
          <tr>
            <th>Path</th>
            <th>Type</th>
            <th>Title</th>
            <th>Description</th>
            <th>Score</th>
          </tr>
        </thead>
        <tbody>
          {hits.map((hit) => (
            <tr key={hit.path}>
              <td className="font-mono whitespace-nowrap">
                <Link
                  to="/concepts/$"
                  params={{ _splat: hit.path }}
                  search={{ tenant }}
                  className={PATH_LINK}
                >
                  {hit.path}
                </Link>
              </td>
              <td className="text-fg-muted">{hit.type}</td>
              <td>{hit.title}</td>
              <td className="text-fg-muted">{hit.description}</td>
              <td className="tabular-nums text-fg-muted">{hit.score.toFixed(2)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
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
  if (isLoading) return <div className={SKELETON} />
  if (!hits || hits.length === 0) return <p className={EMPTY}>No matches.</p>
  return (
    <div className={`mt-3 ${TABLE_SHELL}`}>
      <table className="tbl">
        <thead>
          <tr>
            <th>Path</th>
            <th>Snippet</th>
          </tr>
        </thead>
        <tbody>
          {hits.map((hit) => (
            <tr key={hit.path}>
              <td className="font-mono whitespace-nowrap">
                <Link
                  to="/concepts/$"
                  params={{ _splat: hit.path }}
                  search={{ tenant }}
                  className={PATH_LINK}
                >
                  {hit.path}
                </Link>
              </td>
              <td className="font-mono text-fg-muted">{hit.snippet}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
