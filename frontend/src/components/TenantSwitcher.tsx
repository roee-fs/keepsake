import { useQuery } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { tenantsTenantsGet } from '../client'
import { tenantPrefix } from './TenantId'

const ALL_TENANTS = ''

/**
 * The selected tenant lives in the `tenant` search param, not component state, so
 * any URL that includes it reproduces the same view. Every other query reads it
 * from there.
 */
export function TenantSwitcher() {
  const { tenant } = useSearch({ strict: false })
  const navigate = useNavigate()

  const { data: tenants } = useQuery({
    queryKey: ['tenants'],
    queryFn: async () => (await tenantsTenantsGet({ throwOnError: true })).data,
  })

  // Sized by its column rather than by its content: left to itself a select is
  // as wide as its longest option, which on the overview is the whole page.
  return (
    <select
      className="w-full max-w-72 rounded-md border border-line bg-surface px-2 py-1.5 font-mono text-fg-muted transition-colors hover:text-fg"
      value={tenant ?? ALL_TENANTS}
      onChange={(event) => {
        const value = event.target.value || undefined
        // Another tenant's page offset can land past this tenant's last page.
        void navigate({ to: '.', search: (prev) => ({ ...prev, tenant: value, offset: undefined }) })
      }}
    >
      <option value={ALL_TENANTS}>All tenants</option>
      {tenants?.map((t) => (
        <option key={t.tenant_id} value={t.tenant_id}>
          {tenantPrefix(t.tenant_id)} ({t.concepts})
        </option>
      ))}
    </select>
  )
}
