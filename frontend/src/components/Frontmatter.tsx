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
    return <p className="text-sm text-gray-500">No frontmatter.</p>
  }
  return (
    <table className="w-full text-sm">
      <tbody>
        {entries.map(([key, value]) => (
          <tr key={key} className="border-t">
            <td className="py-1 pr-4 align-top font-mono text-gray-500">{key}</td>
            <td className="py-1 font-mono break-all">{formatValue(value)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
