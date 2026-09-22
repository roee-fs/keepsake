type StatCardProps = {
  label: string
  value: number | undefined
  /** 'warning' calls out a positive value (e.g. orphans) without hiding it as just another tile. */
  tone?: 'default' | 'warning'
}

export function StatCard({ label, value, tone = 'default' }: StatCardProps) {
  const warn = tone === 'warning' && !!value
  return (
    <div className="rounded-md border border-line bg-surface px-4 py-3">
      <div className="text-sm text-fg-muted">{label}</div>
      <div
        className={`mt-1 text-xl font-semibold tabular-nums ${warn ? 'text-warn' : 'text-fg'}`}
      >
        {value === undefined ? '—' : value.toLocaleString()}
      </div>
    </div>
  )
}
