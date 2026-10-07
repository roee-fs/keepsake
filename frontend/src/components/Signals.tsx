import type { ConceptDetail } from '../client'
import { provenance, type Event } from '../lib/provenance'

type SignalsProps = Pick<ConceptDetail, 'status' | 'stale' | 'trust' | 'verified_stale'>

const BADGE = 'rounded border px-1.5 py-px font-mono text-xs'
const QUIET = `${BADGE} border-line text-fg-muted`
const WARN = `${BADGE} border-warn/40 text-warn`

// Labels a concept with its OKF §5 signals; anything that should give a reader pause is tinted.
export function SignalBadges({ status, stale, trust, verified_stale }: SignalsProps) {
  return (
    <span className="inline-flex flex-wrap gap-1.5">
      <span className={status === 'stable' ? QUIET : WARN}>{status}</span>
      <span className={QUIET}>{trust}</span>
      {verified_stale && (
        <span className={WARN} title="The content changed after the verification that sets its trust.">
          review due
        </span>
      )}
      {stale && (
        <span className={WARN} title="Past its stale_after date.">
          stale
        </span>
      )}
    </span>
  )
}

function EventRow({ label, event }: { label: string; event: Event }) {
  return (
    <tr className="first:border-t-0">
      <td className="w-48 text-fg-muted">{label}</td>
      <td className="font-mono">{event.by}</td>
      <td className="font-mono text-fg-muted">{event.at || '—'}</td>
    </tr>
  )
}

// Who wrote the content and who confirmed it, from frontmatter `generated` and `verified`.
export function ProvenanceList({ frontmatter }: { frontmatter: Record<string, unknown> }) {
  const { generated, verified } = provenance(frontmatter)
  if (!generated && verified.length === 0) {
    return <p className="text-fg-muted">No provenance recorded.</p>
  }
  return (
    <div className="overflow-hidden rounded-md border border-line">
      <table className="tbl">
        <tbody>
          {generated && <EventRow label="Generated" event={generated} />}
          {verified.map((event, i) => (
            <EventRow key={`${event.by}-${event.at}-${i}`} label="Verified" event={event} />
          ))}
        </tbody>
      </table>
    </div>
  )
}
