import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useSearch } from '@tanstack/react-router'
import { activityActivityGet, statsStatsGet, statsTimeseriesStatsTimeseriesGet } from '../client'
import { ActivityList } from '../components/ActivityList'
import { StatCard } from '../components/StatCard'
import { TenantSwitcher } from '../components/TenantSwitcher'
import { WritesChart } from '../components/WritesChart'

export const Route = createFileRoute('/')({
  component: Index,
})

function Index() {
  const { tenant } = useSearch({ strict: false })

  const stats = useQuery({
    queryKey: ['stats', tenant],
    queryFn: async () => (await statsStatsGet({ query: { tenant }, throwOnError: true })).data,
  })

  const timeseries = useQuery({
    queryKey: ['stats-timeseries', tenant],
    queryFn: async () =>
      (await statsTimeseriesStatsTimeseriesGet({ query: { tenant }, throwOnError: true })).data,
  })

  const activity = useQuery({
    queryKey: ['activity', tenant],
    queryFn: async () => (await activityActivityGet({ query: { tenant }, throwOnError: true })).data,
  })

  const totals = stats.data
  // by_type arrives already grouped; counting its keys isn't re-aggregating
  // the counts, just reading how many groups came back.
  const typeCount = totals ? Object.keys(totals.by_type).length : undefined

  return (
    <div className="flex flex-col gap-6 p-4">
      <TenantSwitcher />

      <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
        <StatCard label="Concepts" value={totals?.concepts} />
        <StatCard label="Types" value={typeCount} />
        <StatCard label="Revisions" value={totals?.revisions} />
        <StatCard label="Orphans" value={totals?.orphans} tone="warning" />
      </div>

      <div>
        <h2 className="mb-2 text-sm font-medium text-gray-500">Writes per day</h2>
        <WritesChart data={timeseries.data} isLoading={timeseries.isLoading} />
      </div>

      <div>
        <h2 className="mb-2 text-sm font-medium text-gray-500">Recent activity</h2>
        <ActivityList revisions={activity.data} isLoading={activity.isLoading} />
      </div>
    </div>
  )
}
