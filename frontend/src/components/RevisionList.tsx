import type { RevisionOut } from '../client'

type RevisionListProps = {
  revisions: RevisionOut[]
}

// One concept's history, so unlike ActivityList's cross-path feed there is no
// Path column.
export function RevisionList({ revisions }: RevisionListProps) {
  if (revisions.length === 0) {
    return <p className="text-fg-muted">No revisions.</p>
  }
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="tbl">
        <thead>
          <tr>
            <th>Version</th>
            <th>Op</th>
            <th>Updated by</th>
            <th>When</th>
          </tr>
        </thead>
        <tbody>
          {revisions.map((r) => (
            <tr key={r.version}>
              <td className="font-mono tabular-nums">{r.version}</td>
              <td className="text-fg-muted">{r.op}</td>
              <td className="text-fg-muted">{r.updated_by}</td>
              <td className="tabular-nums whitespace-nowrap text-fg-muted">
                {new Date(r.created_at).toLocaleString()}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
