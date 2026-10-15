import { Area, AreaChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import type { Bucket } from '../client'

export type Unit = 'hour' | 'day' | 'week'

type TimeseriesChartProps = {
  data: Bucket[] | undefined
  series: 'writes' | 'reads'
  unit: Unit
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

/** Hours read as `MM-DD HH:MM`, days and weeks as the date they start. */
function label(start: string, unit: Unit) {
  return unit === 'hour' ? start.slice(5, 16).replace('T', ' ') : start.slice(0, 10)
}

/** `data` arrives bucketed and zero-filled by the backend; an all-zero window
 * renders as an empty state, not an axis with a flat line. */
export function TimeseriesChart({ data, series, unit, isLoading, isError }: TimeseriesChartProps) {
  if (isLoading) {
    return <div className="h-48 animate-pulse rounded-md border border-line bg-surface" />
  }
  const hasAny = data !== undefined && data.some((d) => d[series] > 0)
  if (isError || !hasAny) {
    return (
      <div className="flex h-48 items-center justify-center rounded-md border border-line bg-surface text-fg-muted">
        {/* A failed query settles with no data, which is indistinguishable from a
            quiet window unless the error state is checked first. */}
        {isError ? `Could not load ${series}.` : `No ${series} in this window.`}
      </div>
    )
  }
  const fill = `${series}-fill`
  return (
    <div className="h-48 rounded-md border border-line bg-surface p-3">
      <ResponsiveContainer width="100%" height="100%">
        <AreaChart data={data} margin={{ top: 4, right: 4, bottom: 0, left: 0 }}>
          <defs>
            <linearGradient id={fill} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" stopColor="var(--color-accent)" stopOpacity={0.35} />
              <stop offset="100%" stopColor="var(--color-accent)" stopOpacity={0} />
            </linearGradient>
          </defs>
          <CartesianGrid stroke="var(--color-line)" vertical={false} />
          <XAxis
            dataKey="start"
            tickFormatter={(start: string) => label(start, unit)}
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
            labelFormatter={(start) => label(String(start), unit)}
            contentStyle={TOOLTIP_CONTENT}
            labelStyle={{ color: 'var(--color-fg-muted)' }}
            itemStyle={{ color: 'var(--color-fg)' }}
            cursor={{ stroke: 'var(--color-fg-faint)', strokeWidth: 1 }}
          />
          <Area
            type="monotone"
            dataKey={series}
            stroke="var(--color-accent)"
            strokeWidth={1.5}
            fill={`url(#${fill})`}
          />
        </AreaChart>
      </ResponsiveContainer>
    </div>
  )
}
