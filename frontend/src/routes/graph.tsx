import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useSearch } from '@tanstack/react-router'
import { graphGraphGet } from '../client'
import { ConceptGraph } from '../components/ConceptGraph'
import { TenantSwitcher } from '../components/TenantSwitcher'

export const Route = createFileRoute('/graph')({
  component: GraphPage,
})

function GraphPage() {
  const { tenant } = useSearch({ strict: false })

  const graph = useQuery({
    queryKey: ['graph', tenant],
    queryFn: async () =>
      // Safe: `enabled` keeps this from running without a tenant.
      (await graphGraphGet({ query: { tenant: tenant! }, throwOnError: true })).data,
    enabled: !!tenant,
  })

  return (
    <div className="flex flex-col gap-6">
      <TenantSwitcher />
      {!tenant ? (
        // Links resolve within one tenant, so a graph across tenants has no edges to draw.
        <div className="text-fg-muted">Pick a tenant to see how its concepts link.</div>
      ) : graph.isLoading ? (
        <div className="h-[640px] animate-pulse rounded-md border border-line bg-surface" />
      ) : graph.isError || !graph.data ? (
        <div className="text-fg-muted">Could not load the graph.</div>
      ) : graph.data.nodes.length === 0 ? (
        <div className="text-fg-muted">This tenant has no concepts.</div>
      ) : (
        <>
          {graph.data.truncated && (
            <div className="text-warn">
              The graph is cut off at {graph.data.nodes.length} nodes and {graph.data.edges.length} links. Concepts and links past that are hidden.
            </div>
          )}
          <ConceptGraph graph={graph.data} tenant={tenant} />
        </>
      )}
    </div>
  )
}
