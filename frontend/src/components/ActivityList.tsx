import type { RevisionOut } from '../client'

type ActivityListProps = {
  revisions: RevisionOut[] | undefined
  isLoading: boolean
  // A path is unique only within a tenant, so two tenants can write the same
  // path -- the column only earns its place once rows can span tenants.
  showTenant: boolean
}

export function ActivityList({ revisions, isLoading, showTenant }: ActivityListProps) {
  if (isLoading) {
    return <div className="h-48 animate-pulse rounded border bg-gray-100" />
  }
  if (!revisions || revisions.length === 0) {
    return <div className="rounded border p-4 text-sm text-gray-500">No activity yet.</div>
  }
  return (
    <table className="w-full text-sm">
      <thead>
        <tr className="text-left text-gray-500">
          <th className="py-1 pr-2 font-medium">Path</th>
          {showTenant && <th className="py-1 pr-2 font-medium">Tenant</th>}
          <th className="py-1 pr-2 font-medium">Version</th>
          <th className="py-1 pr-2 font-medium">Op</th>
          <th className="py-1 pr-2 font-medium">Updated by</th>
          <th className="py-1 font-medium">When</th>
        </tr>
      </thead>
      <tbody>
        {revisions.map((r) => (
          <tr key={`${r.tenant_id}:${r.path}:${r.version}`} className="border-t">
            <td className="py-1 pr-2 font-mono">{r.path}</td>
            {showTenant && <td className="py-1 pr-2 font-mono">{r.tenant_id}</td>}
            <td className="py-1 pr-2">{r.version}</td>
            <td className="py-1 pr-2">{r.op}</td>
            <td className="py-1 pr-2">{r.updated_by}</td>
            <td className="py-1">{new Date(r.created_at).toLocaleString()}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
