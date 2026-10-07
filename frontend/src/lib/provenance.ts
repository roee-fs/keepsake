export type Event = { by: string; at: string }

export type Provenance = { generated: Event | null; verified: Event[] }

function event(value: unknown): Event | null {
  if (typeof value !== 'object' || value === null) return null
  const { by, at } = value as Record<string, unknown>
  if (typeof by !== 'string' || by === '') return null
  return { by, at: typeof at === 'string' ? at : '' }
}

// Who wrote a concept and who confirmed it, read from OKF §5.2 frontmatter.
export function provenance(frontmatter: Record<string, unknown>): Provenance {
  const verified = frontmatter.verified
  const events = Array.isArray(verified) ? verified : verified === undefined ? [] : [verified]
  return {
    generated: event(frontmatter.generated),
    verified: events.map(event).filter((e): e is Event => e !== null),
  }
}
