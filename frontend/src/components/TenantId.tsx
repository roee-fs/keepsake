const PREFIX_LENGTH = 8

/**
 * Tenant UUIDs are seeded and compared by their leading segment; the remaining 28
 * characters are noise in a table row. Exported separately from the component
 * because an `<option>` takes text, not markup, and both must truncate alike.
 */
export function tenantPrefix(tenantId: string): string {
  return tenantId.slice(0, PREFIX_LENGTH)
}

export function TenantId({ tenantId }: { tenantId: string }) {
  return (
    <span className="font-mono text-fg-muted" title={tenantId}>
      {tenantPrefix(tenantId)}
    </span>
  )
}
