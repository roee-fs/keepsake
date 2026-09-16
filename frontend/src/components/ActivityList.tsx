import type { RevisionOut } from '../client'

type ActivityListProps = {
  revisions: RevisionOut[] | undefined
  isLoading: boolean
}

// RevisionOut carries no tenant_id -- the API has nothing to put in a tenant
// column here, in all-tenants mode or otherwise.
export function ActivityList({ revisions, isLoading }: ActivityListProps) {
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
          <th className="py-1 pr-2 font-medium">Version</th>
          <th className="py-1 pr-2 font-medium">Op</th>
          <th className="py-1 pr-2 font-medium">Updated by</th>
          <th className="py-1 font-medium">When</th>
        </tr>
      </thead>
      <tbody>
        {revisions.map((r) => (
          <tr key={`${r.path}:${r.version}`} className="border-t">
            <td className="py-1 pr-2 font-mono">{r.path}</td>
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
