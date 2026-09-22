import type { RevisionOut } from '../client'
import { TenantId } from './TenantId'

type ActivityListProps = {
  revisions: RevisionOut[] | undefined
  isLoading: boolean
  // A path is unique only within a tenant, so two tenants can write the same
  // path -- the column only earns its place once rows can span tenants.
  showTenant: boolean
}

export function ActivityList({ revisions, isLoading, showTenant }: ActivityListProps) {
  if (isLoading) {
    return <div className="h-48 animate-pulse rounded-md border border-line bg-surface" />
  }
  if (!revisions || revisions.length === 0) {
    return (
      <div className="rounded-md border border-line bg-surface p-4 text-fg-muted">
        No activity yet.
      </div>
    )
  }
  return (
    <div className="overflow-x-auto rounded-md border border-line">
      <table className="tbl">
        <thead>
          <tr>
            <th>Path</th>
            {showTenant && <th>Tenant</th>}
            <th>Version</th>
            <th>Op</th>
            <th>Updated by</th>
            <th>When</th>
          </tr>
        </thead>
        <tbody>
          {revisions.map((r) => (
            <tr key={`${r.tenant_id}:${r.path}:${r.version}`}>
              <td className="font-mono">{r.path}</td>
              {showTenant && (
                <td>
                  <TenantId tenantId={r.tenant_id} />
                </td>
              )}
              <td className="font-mono tabular-nums text-fg-muted">{r.version}</td>
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
