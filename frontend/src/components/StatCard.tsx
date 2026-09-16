type StatCardProps = {
  label: string
  value: number | undefined
  /** 'warning' calls out a positive value (e.g. orphans) without hiding it as just another tile. */
  tone?: 'default' | 'warning'
}

export function StatCard({ label, value, tone = 'default' }: StatCardProps) {
  const warn = tone === 'warning' && !!value
  return (
    <div className="rounded border p-4">
      <div className="text-sm text-gray-500">{label}</div>
      <div className={`text-2xl font-semibold ${warn ? 'text-amber-600' : ''}`}>
        {value === undefined ? '—' : value.toLocaleString()}
      </div>
    </div>
  )
}
