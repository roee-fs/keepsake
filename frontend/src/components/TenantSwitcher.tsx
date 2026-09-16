import { useQuery } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { tenantsTenantsGet } from '../client'

const ALL_TENANTS = ''

/**
 * The selected tenant lives in the `tenant` search param, not component state,
 * so any URL that includes it reproduces the same view. Every other query
 * reads the same param instead of tracking its own copy of the selection.
 */
export function TenantSwitcher() {
  const { tenant } = useSearch({ strict: false })
  const navigate = useNavigate()

  const { data: tenants } = useQuery({
    queryKey: ['tenants'],
    queryFn: async () => (await tenantsTenantsGet({ throwOnError: true })).data,
  })

  return (
    <select
      className="rounded border px-2 py-1"
      value={tenant ?? ALL_TENANTS}
      onChange={(event) => {
        const value = event.target.value || undefined
        void navigate({ to: '.', search: (prev) => ({ ...prev, tenant: value }) })
      }}
    >
      <option value={ALL_TENANTS}>All tenants</option>
      {tenants?.map((t) => (
        <option key={t.tenant_id} value={t.tenant_id}>
          {t.tenant_id} ({t.concepts})
        </option>
      ))}
    </select>
  )
}
