type FrontmatterProps = {
  frontmatter: Record<string, unknown>
}

function formatValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'string') return value
  return JSON.stringify(value)
}

export function Frontmatter({ frontmatter }: FrontmatterProps) {
  const entries = Object.entries(frontmatter)
  if (entries.length === 0) {
    return <p className="text-fg-muted">No frontmatter.</p>
  }
  return (
    <div className="overflow-hidden rounded-md border border-line">
      <table className="tbl">
        <tbody>
          {entries.map(([key, value]) => (
            <tr key={key} className="first:border-t-0">
              <td className="w-48 font-mono text-fg-muted">{key}</td>
              <td className="font-mono break-all">{formatValue(value)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}
