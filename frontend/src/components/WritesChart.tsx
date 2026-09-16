import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import type { DailyWrite } from '../client'

type WritesChartProps = {
  data: DailyWrite[] | undefined
  isLoading: boolean
}

/** `data` arrives already bucketed and zero-filled by the backend; an all-zero
 * window (fresh install) is rendered as an empty state, not an axis with a flat line. */
export function WritesChart({ data, isLoading }: WritesChartProps) {
  if (isLoading) {
    return <div className="h-48 animate-pulse rounded border bg-gray-100" />
  }
  const hasWrites = data !== undefined && data.some((d) => d.count > 0)
  if (!hasWrites) {
    return (
      <div className="flex h-48 items-center justify-center rounded border text-sm text-gray-500">
        No writes in this window.
      </div>
    )
  }
  return (
    <div className="h-48 rounded border p-2">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data}>
          <CartesianGrid strokeDasharray="3 3" />
          <XAxis dataKey="date" tick={{ fontSize: 12 }} />
          <YAxis allowDecimals={false} tick={{ fontSize: 12 }} width={32} />
          <Tooltip />
          <Area type="monotone" dataKey="count" stroke="#2563eb" fill="#93c5fd" />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}
