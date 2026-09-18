import type { RevisionOut } from '../client'

type RevisionListProps = {
  revisions: RevisionOut[]
}

// The path is fixed by the caller (one concept's history), so unlike
// ActivityList's cross-path feed, there's no Path column to show.
export function RevisionList({ revisions }: RevisionListProps) {
  if (revisions.length === 0) {
    return <p className="text-sm text-gray-500">No revisions.</p>
  }
  return (
    <table className="w-full text-sm">
      <thead>
        <tr className="text-left text-gray-500">
          <th className="py-1 pr-2 font-medium">Version</th>
          <th className="py-1 pr-2 font-medium">Op</th>
          <th className="py-1 pr-2 font-medium">Updated by</th>
          <th className="py-1 font-medium">When</th>
        </tr>
      </thead>
      <tbody>
        {revisions.map((r) => (
          <tr key={r.version} className="border-t">
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
