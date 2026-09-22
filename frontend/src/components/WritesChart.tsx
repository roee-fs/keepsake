import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import type { DailyWrite } from '../client'

type WritesChartProps = {
  data: DailyWrite[] | undefined
  isLoading: boolean
  isError: boolean
}

// Recharts writes SVG presentation attributes and inline styles, both of which
// resolve CSS custom properties, so the chart reads the same palette as the rest.
const AXIS_TICK = { fontSize: 11, fill: 'var(--color-fg-muted)' }
const AXIS_LINE = { stroke: 'var(--color-line)' }
const TOOLTIP_CONTENT = {
  backgroundColor: 'var(--color-surface)',
  border: '1px solid var(--color-line)',
  borderRadius: '6px',
  fontSize: '12px',
  padding: '6px 10px',
}

/** `data` arrives bucketed and zero-filled by the backend; an all-zero window
 * renders as an empty state, not an axis with a flat line. */
export function WritesChart({ data, isLoading, isError }: WritesChartProps) {
  if (isLoading) {
    return <div className="h-48 animate-pulse rounded-md border border-line bg-surface" />
  }
  const hasWrites = data !== undefined && data.some((d) => d.count > 0)
  if (isError || !hasWrites) {
    return (
      <div className="flex h-48 items-center justify-center rounded-md border border-line bg-surface text-fg-muted">
        {/* A failed query settles with no data, which is indistinguishable from a
            quiet window unless the error state is checked first. */}
        {isError ? 'Could not load writes.' : 'No writes in this window.'}
      </div>
    )
  }
  return (
    <div className="h-48 rounded-md border border-line bg-surface p-3">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: 0 }}>
          <defs>
            <linearGradient id="writes-fill" x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--color-accent)" stopOpacity={0.35} />
              <stop offset="100%" stopColor="var(--color-accent)" stopOpacity={0} />
            </linearGradient>
          </defs>
          <CartesianGrid stroke="var(--color-line)" vertical={false} />
          <XAxis
            dataKey="date"
            tick={AXIS_TICK}
            tickLine={false}
            axisLine={AXIS_LINE}
            tickMargin={8}
          />
          <YAxis
            allowDecimals={false}
            tick={AXIS_TICK}
            tickLine={false}
            axisLine={false}
            width={32}
          />
          <Tooltip
            contentStyle={TOOLTIP_CONTENT}
            labelStyle={{ color: 'var(--color-fg-muted)' }}
            itemStyle={{ color: 'var(--color-fg)' }}
            cursor={{ stroke: 'var(--color-fg-faint)', strokeWidth: 1 }}
          />
          <Area
            type="monotone"
            dataKey="count"
            stroke="var(--color-accent)"
            strokeWidth={1.5}
            fill="url(#writes-fill)"
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}
