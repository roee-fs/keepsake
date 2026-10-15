import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useSearch } from '@tanstack/react-router'
import { useState } from 'react'
import { activityActivityGet, statsStatsGet, statsTimeseriesStatsTimeseriesGet } from '../client'
import { ActivityList } from '../components/ActivityList'
import { StatCard } from '../components/StatCard'
import { TenantSwitcher } from '../components/TenantSwitcher'
import { TimeseriesChart, type Unit } from '../components/TimeseriesChart'

// About two days of hours, a month of days and half a year of weeks.
const BUCKETS: Record<Unit, number> = { hour: 48, day: 30, week: 26 }

export const Route = createFileRoute('/')({
  component: Index,
})

function Index() {
  const { tenant } = useSearch({ strict: false })
  const [unit, setUnit] = useState<Unit>('day')

  const stats = useQuery({
    queryKey: ['stats', tenant],
    queryFn: async () => (await statsStatsGet({ query: { tenant }, throwOnError: true })).data,
  })

  const timeseries = useQuery({
    queryKey: ['stats-timeseries', tenant, unit],
    queryFn: async () =>
      (
        await statsTimeseriesStatsTimeseriesGet({
          query: { tenant, bucket: unit, buckets: BUCKETS[unit] },
          throwOnError: true,
        })
      ).data,
  })

  const activity = useQuery({
    queryKey: ['activity', tenant],
    queryFn: async () => (await activityActivityGet({ query: { tenant }, throwOnError: true })).data,
  })

  const totals = stats.data
  const typeCount = totals ? Object.keys(totals.by_type).length : undefined

  return (
    <div className="flex flex-col gap-8">
      <TenantSwitcher />

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <StatCard label="Concepts" value={totals?.concepts} />
        <StatCard label="Types" value={typeCount} />
        <StatCard label="Revisions" value={totals?.revisions} />
        <StatCard label="Orphans" value={totals?.orphans} tone="warning" />
      </div>

      <div>
        <div className="mb-3 flex items-center justify-between gap-3">
          <h2 className="text-md font-semibold tracking-tight">Writes and reads</h2>
          <select
            aria-label="Bucket size"
            className="rounded-md border border-line bg-surface px-2 py-1.5 text-fg-muted transition-colors hover:text-fg"
            value={unit}
            onChange={(event) => setUnit(event.target.value as Unit)}
          >
            <option value="hour">Per hour</option>
            <option value="day">Per day</option>
            <option value="week">Per week</option>
          </select>
        </div>
        <div className="grid gap-3 sm:grid-cols-2">
          {(['writes', 'reads'] as const).map((series) => (
            <div key={series}>
              <h3 className="mb-2 text-sm text-fg-muted">
                {series === 'writes' ? 'Writes' : 'Reads'} per {unit} (UTC)
              </h3>
              <TimeseriesChart
                data={timeseries.data}
                series={series}
                unit={unit}
                isLoading={timeseries.isLoading}
                isError={timeseries.isError}
              />
            </div>
          ))}
        </div>
      </div>

      <div>
        <h2 className="mb-3 text-md font-semibold tracking-tight">Recent activity</h2>
        <ActivityList
          revisions={activity.data}
          isLoading={activity.isLoading}
          isError={activity.isError}
          showTenant={tenant === undefined}
        />
      </div>
    </div>
  )
}
